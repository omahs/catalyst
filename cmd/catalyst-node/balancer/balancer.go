package balancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sync"

	glog "github.com/magicsong/color-glog"
)

type Config struct {
	Args             []string
	MistUtilLoadPort uint32
}

type Balancer struct {
	config *Config
	cmd    *exec.Cmd
}

// create a new load balancer instance
func NewBalancer(config *Config) *Balancer {
	return &Balancer{
		config: config,
		cmd:    nil,
	}
}

// start this load balancer instance, execing MistUtilLoad if necessary
func (b *Balancer) Start() error {
	return b.execBalancer(b.config.Args)
}

func (b *Balancer) changeLoadBalancerServers(endpoint, tmpl, server, action string) ([]byte, error) {
	serverTmpl := fmt.Sprintf(tmpl, server)
	actionURL := endpoint + "?" + action + "server=" + url.QueryEscape(serverTmpl)
	req, err := http.NewRequest("POST", actionURL, nil)
	if err != nil {
		glog.Errorf("Error creating request: %v", err)
		return nil, err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		glog.Errorf("Error making request: %v", err)
		return nil, err
	}

	bytes, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		glog.Errorf("Error reading response: %v", err)
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		glog.Errorf("Error response from load balancer changing servers: %s\n", string(bytes))
		return bytes, errors.New(string(bytes))
	}

	glog.V(6).Infof("requested mist to %s server %s to the load balancer\n", action, server)
	glog.V(6).Info(string(bytes))
	return bytes, nil
}

func (b *Balancer) getMistLoadBalancerServers(endpoint string) (map[string]interface{}, error) {
	url := endpoint + "?lstservers=1"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		glog.Errorf("Error creating request: %v", err)
		return nil, err
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		glog.Errorf("Error making request: %v", err)
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)
		glog.Errorf("Error response from load balancer listing servers: %s\n", string(b))
		return nil, errors.New(string(b))
	}
	bytes, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		glog.Errorf("Error reading response: %v", err)
		return nil, err
	}

	var mistResponse map[string]interface{}

	json.Unmarshal([]byte(string(bytes)), &mistResponse)

	return mistResponse, nil
}

func (b *Balancer) Kill() {
	glog.Infof("killing MistUtilLoad")
	b.cmd.Process.Kill()
}

func (b *Balancer) execBalancer(balancerArgs []string) error {
	args := append(balancerArgs, "-p", fmt.Sprintf("%d", b.config.MistUtilLoadPort))
	glog.Infof("Running MistUtilLoad with %v", args)
	b.cmd = exec.Command("MistUtilLoad", args...)

	b.cmd.Stdout = os.Stdout
	b.cmd.Stderr = os.Stderr

	err := b.cmd.Start()
	if err != nil {
		return err
	}

	err = b.cmd.Wait()

	return err
}

func (b *Balancer) queryMistForClosestNode(playbackID, lat, lon, prefix string) (string, error) {
	// First, check to see if any server has this stream
	_, err1 := b.QueryMistForClosestNodeSource(playbackID, lat, lon, prefix, true)
	// Then, check the best playback server
	node, err2 := b.QueryMistForClosestNodeSource(playbackID, lat, lon, prefix, false)
	// If we can't get a playback server, error
	if err2 != nil {
		return "", err2
	}
	// If we didn't find the stream but we did find a node, return it with the error for 404s
	if err1 != nil {
		return node, err1
	}
	// Good path, we found the stream and a playback nodew!
	return node, nil
}

// return the best node available for a given stream. will return any node if nobody has the stream.
func (b *Balancer) GetBestNode(redirectPrefixes []string, playbackID, lat, lon, fallbackPrefix string) (string, string, error) {
	var nodeAddr, fullPlaybackID, fallbackAddr string
	var mu sync.Mutex
	var err error
	var waitGroup sync.WaitGroup

	for _, prefix := range redirectPrefixes {
		waitGroup.Add(1)
		go func(prefix string) {
			addr, e := b.queryMistForClosestNode(playbackID, lat, lon, prefix)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				err = e
				glog.V(8).Infof("error finding origin server playbackID=%s prefix=%s error=%s", playbackID, prefix, e)
				// If we didn't find a stream but we did find a server, keep that so we can use it to handle a 404
				if addr != "" {
					fallbackAddr = addr
				}
			} else {
				nodeAddr = addr
				fullPlaybackID = prefix + "+" + playbackID
			}
			waitGroup.Done()
		}(prefix)
	}
	waitGroup.Wait()

	// good path: we found the stream and a good node to play it back, yay!
	if nodeAddr != "" {
		return nodeAddr, fullPlaybackID, nil
	}

	// bad path: nobody has the stream, but we did find a server which can handle the 404 for us.
	if fallbackAddr != "" {
		if fallbackPrefix == "" {
			fallbackPrefix = redirectPrefixes[0]
		}
		return fallbackAddr, fallbackPrefix + "+" + playbackID, nil
	}

	// ugly path: we couldn't find ANY servers. yikes.
	return "", "", err
}

func (b *Balancer) QueryMistForClosestNodeSource(playbackID, lat, lon, prefix string, source bool) (string, error) {
	if prefix != "" {
		prefix += "+"
	}
	var murl string
	enc := url.QueryEscape(fmt.Sprintf("%s%s", prefix, playbackID))
	if source {
		murl = fmt.Sprintf("http://localhost:%d/?source=%s", b.config.MistUtilLoadPort, enc)
	} else {
		murl = fmt.Sprintf("http://localhost:%d/%s", b.config.MistUtilLoadPort, enc)
	}
	glog.V(8).Infof("MistUtilLoad started request=%s", murl)
	req, err := http.NewRequest("GET", murl, nil)
	if err != nil {
		return "", err
	}
	if lat != "" && lon != "" {
		req.Header.Set("X-Latitude", lat)
		req.Header.Set("X-Longitude", lon)
	} else {
		glog.Warningf("Incoming request missing X-Latitude/X-Longitude, response will not be geolocated")
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET request '%s' failed with http status code %d", murl, resp.StatusCode)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("GET request '%s' failed while reading response body", murl)
	}
	glog.V(8).Infof("MistUtilLoad responded request=%s response=%s", murl, body)
	if string(body) == "FULL" {
		return "", fmt.Errorf("GET request '%s' returned 'FULL'", murl)
	}
	return string(body), nil
}
