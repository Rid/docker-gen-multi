package dockergen

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsouza/go-dockerclient"
)

type generator struct {
	Clients                     []*docker.Client
	Configs                    ConfigFile
	Endpoints                   []string
	TLSVerify                  bool
	TLSCert, TLSCaCert, TLSKey string
	All                        bool

	wg    sync.WaitGroup
	retry bool
}

type GeneratorConfig struct {
	Endpoints string

	TLSCert   string
	TLSKey    string
	TLSCACert string
	TLSVerify bool
	All       bool

	ConfigFile ConfigFile
}

func NewGenerator(gc GeneratorConfig) (*generator, error) {
	var apiVersion *docker.Env
	var clients []*docker.Client
	var endpoints []string

	for _, oneEndpoint := range strings.Split(gc.Endpoints, ",") {
		endpoint, err := GetEndpoint(oneEndpoint)
		if err != nil {
			return nil, fmt.Errorf("Bad endpoint: %s", err)
		}

		client, err := NewDockerClient(endpoint, gc.TLSVerify, gc.TLSCert, gc.TLSCACert, gc.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("Unable to create docker client: %s", err)
		}

		clients = append(clients, client)
		endpoints = append(endpoints, endpoint)

		apiVersion, err = client.Version()
		if err != nil {
			log.Printf("Error retrieving docker server version info: %s\n", err)
		}
	}

	// Grab the docker daemon info once and hold onto it
	SetDockerEnv(apiVersion)

	return &generator{
		Clients:    clients,
		Endpoints:  endpoints,
		TLSVerify: gc.TLSVerify,
		TLSCert:   gc.TLSCert,
		TLSCaCert: gc.TLSCACert,
		TLSKey:    gc.TLSKey,
		All:       gc.All,
		Configs:   gc.ConfigFile,
		retry:     true,
	}, nil
}

func (g *generator) Generate() error {
	g.generateFromContainers()
	g.generateAtInterval()
	g.generateFromEvents()
	g.generateFromSignals()
	g.wg.Wait()

	return nil
}

func (g *generator) generateFromSignals() {
	var hasWatcher bool
	for _, config := range g.Configs.Config {
		if config.Watch {
			hasWatcher = true
			break
		}
	}

	// If none of the configs need to watch for events, don't watch for signals either
	if !hasWatcher {
		return
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		sigChan := newSignalChannel()
		for {
			sig := <-sigChan
			log.Printf("Received signal: %s\n", sig)
			switch sig {
			case syscall.SIGHUP:
				g.generateFromContainers()
			case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
				// exit when context is done
				return
			}
		}
	}()
}

func (g *generator) generateFromContainers() {
	containers, err := g.getContainers()
	if err != nil {
		log.Printf("Error listing containers: %s\n", err)
		return
	}
	for _, config := range g.Configs.Config {
		changed := GenerateFile(config, containers)
		if !changed {
			log.Printf("Contents of %s did not change. Skipping notification '%s'", config.Dest, config.NotifyCmd)
			continue
		}
		g.runNotifyCmd(config)
		g.sendSignalToContainer(config)
	}
}

func (g *generator) generateAtInterval() {
	for _, config := range g.Configs.Config {

		if config.Interval == 0 {
			continue
		}

		log.Printf("Generating every %d seconds", config.Interval)
		g.wg.Add(1)
		ticker := time.NewTicker(time.Duration(config.Interval) * time.Second)
		go func(config Config) {
			defer g.wg.Done()

			sigChan := newSignalChannel()
			for {
				select {
				case <-ticker.C:
					containers, err := g.getContainers()
					if err != nil {
						log.Printf("Error listing containers: %s\n", err)
						continue
					}
				// ignore changed return value. always run notify command
					GenerateFile(config, containers)
					g.runNotifyCmd(config)
					g.sendSignalToContainer(config)
				case sig := <-sigChan:
					log.Printf("Received signal: %s\n", sig)
					switch sig {
					case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
						ticker.Stop()
						return
					}
				}
			}
		}(config)
	}
}

func (g *generator) generateFromEvents() {
	configs := g.Configs.FilterWatches()
	if len(configs.Config) == 0 {
		return
	}

	var watchers []chan *docker.APIEvents

	for _, config := range configs.Config {

		if !config.Watch {
			continue
		}

		g.wg.Add(1)

		go func(config Config, watcher chan *docker.APIEvents) {
			defer g.wg.Done()
			watchers = append(watchers, watcher)

			debouncedChan := newDebounceChannel(watcher, config.Wait)
			for range debouncedChan {
				containers, err := g.getContainers()
				if err != nil {
					log.Printf("Error listing containers: %s\n", err)
					continue
				}
				changed := GenerateFile(config, containers)
				if !changed {
					log.Printf("Contents of %s did not change. Skipping notification '%s'", config.Dest, config.NotifyCmd)
					continue
				}
				g.runNotifyCmd(config)
				g.sendSignalToContainer(config)
			}
		}(config, make(chan *docker.APIEvents, 100))
	}

	log.Printf("Watching %d endpoints", len(g.Clients))

	// maintains docker client connection and passes events to watchers
	go func() {
		// channel will be closed by go-dockerclient
		eventChan := make(chan *docker.APIEvents, 100)
		sigChan := newSignalChannel()

		timeout := time.Duration(10000 / len(g.Clients)) * time.Millisecond

		log.Printf("Pinging daemons every %v", timeout)

		for {
			watching := make([]bool, len(g.Clients))

			clientNum := 0
			for _, client := range g.Clients {
				watching[clientNum] = false
				if client == nil {
					endpoint, err := GetEndpoint(g.Endpoints[clientNum])
					if err != nil {
						log.Printf("Bad endpoint: %s", err)
						time.Sleep(10 * time.Second)
						continue
					}

					client, err = NewDockerClient(endpoint, g.TLSVerify, g.TLSCert, g.TLSCaCert, g.TLSKey)
					if err != nil {
						log.Printf("Unable to connect to docker daemon: %s", err)
						time.Sleep(10 * time.Second)
						continue
					}
				}
				clientNum += 1
			}

			for {
				clientNum := 0
				for _, client := range g.Clients {
					if client == nil {
						break
					}

					if !watching[clientNum] {
						err := client.AddEventListener(eventChan)
						if err != nil && err != docker.ErrListenerAlreadyExists {
							log.Printf("Error registering docker event listener: %s", err)
							time.Sleep(10 * time.Second)
							continue
						}
						watching[clientNum] = true
						log.Println("Watching docker events")
						// sync all configs after resuming listener
						g.generateFromContainers()
					}
					select {
					case event, ok := <-eventChan:
						if !ok {
							log.Printf("Docker daemon connection interrupted")
							if watching[clientNum] {
								client.RemoveEventListener(eventChan)
								watching[clientNum] = false
								client = nil
							}
							if !g.retry {
								// close all watchers and exit
								for _, watcher := range watchers {
									close(watcher)
								}
								return
							}
							// recreate channel and attempt to resume
							eventChan = make(chan *docker.APIEvents, 100)
							time.Sleep(10 * time.Second)
							break
						}
						if event.Status == "start" || event.Status == "stop" || event.Status == "die" {
							log.Printf("Received event %s for container %s", event.Status, event.ID[:12])
							// fanout event to all watchers
							for _, watcher := range watchers {
								watcher <- event
							}
						}
					case <-time.After(timeout):
					// check for docker liveness
						err := client.Ping()
						if err != nil {
							log.Printf("Unable to ping docker daemon: %s", err)
							if watching[clientNum] {
								client.RemoveEventListener(eventChan)
								watching[clientNum] = false
								client = nil
							}
						}
					case sig := <-sigChan:
						log.Printf("Received signal: %s\n", sig)
						switch sig {
						case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
							// close all watchers and exit
							for _, watcher := range watchers {
								close(watcher)
							}
							return
						}
					}
					clientNum += 1
				}
			}
		}
	}()
}

func (g *generator) runNotifyCmd(config Config) {
	if config.NotifyCmd == "" {
		return
	}

	log.Printf("Running '%s'", config.NotifyCmd)
	cmd := exec.Command("/bin/sh", "-c", config.NotifyCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error running notify command: %s, %s\n", config.NotifyCmd, err)
	}
	if config.NotifyOutput {
		for _, line := range strings.Split(string(out), "\n") {
			if line != "" {
				log.Printf("[%s]: %s", config.NotifyCmd, line)
			}
		}
	}
}

func (g *generator) sendSignalToContainer(config Config) {
	if len(config.NotifyContainers) < 1 {
		return
	}

	for container, signal := range config.NotifyContainers {
		log.Printf("Sending container '%s' signal '%v'", container, signal)
		killOpts := docker.KillContainerOptions{
			ID:     container,
			Signal: signal,
		}
		if err := g.Clients[0].KillContainer(killOpts); err != nil {
			log.Printf("Error sending signal to container: %s", err)
		}
	}
}

func (g *generator) getContainers() ([]*RuntimeContainer, error) {
	containers := []*RuntimeContainer{}

	for _, client := range g.Clients {

		apiInfo, err := client.Info()
		if err != nil {
			log.Printf("Error retrieving docker server info: %s\n", err)
			// We should exit here as if docker is down the socket will be recreated when it comes back up
			// therefore restart of docker-gen is needed
			fmt.Fprintf(os.Stderr, "error: %s\n", err)
			os.Exit(1)
		} else {
			SetServerInfo(apiInfo)
		}

		apiContainers, err := client.ListContainers(docker.ListContainersOptions{
			All:  g.All,
			Size: false,
		})
		if err != nil {
			return nil, err
		}

		for _, apiContainer := range apiContainers {
			container, err := client.InspectContainer(apiContainer.ID)
			if err != nil {
				log.Printf("Error inspecting container: %s: %s\n", apiContainer.ID, err)
				continue
			}

			registry, repository, tag := splitDockerImage(container.Config.Image)
			runtimeContainer := &RuntimeContainer{
				ID: container.ID,
				Image: DockerImage{
					Registry:   registry,
					Repository: repository,
					Tag:        tag,
				},
				State: State{
					Running: container.State.Running,
				},
				Name:         strings.TrimLeft(container.Name, "/"),
				Hostname:     container.Config.Hostname,
				Gateway:      container.NetworkSettings.Gateway,
				Addresses:    []Address{},
				Networks:     []Network{},
				Env:          make(map[string]string),
				Volumes:      make(map[string]Volume),
				Node:         SwarmNode{},
				Labels:       make(map[string]string),
				IP:           container.NetworkSettings.IPAddress,
				IP6LinkLocal: container.NetworkSettings.LinkLocalIPv6Address,
				IP6Global:    container.NetworkSettings.GlobalIPv6Address,
			}
			for k, v := range container.NetworkSettings.Ports {
				address := Address{
					IP:           container.NetworkSettings.IPAddress,
					IP6LinkLocal: container.NetworkSettings.LinkLocalIPv6Address,
					IP6Global:    container.NetworkSettings.GlobalIPv6Address,
					Port:         k.Port(),
					Proto:        k.Proto(),
				}
				if len(v) > 0 {
					address.HostPort = v[0].HostPort
					address.HostIP = v[0].HostIP
				}
				runtimeContainer.Addresses = append(runtimeContainer.Addresses,
					address)

			}
			for k, v := range container.NetworkSettings.Networks {
				network := Network{
					IP:                  v.IPAddress,
					Name:                k,
					Gateway:             v.Gateway,
					EndpointID:          v.EndpointID,
					IPv6Gateway:         v.IPv6Gateway,
					GlobalIPv6Address:   v.GlobalIPv6Address,
					MacAddress:          v.MacAddress,
					GlobalIPv6PrefixLen: v.GlobalIPv6PrefixLen,
					IPPrefixLen:         v.IPPrefixLen,
				}

				runtimeContainer.Networks = append(runtimeContainer.Networks,
					network)
			}
			for k, v := range container.Volumes {
				runtimeContainer.Volumes[k] = Volume{
					Path:      k,
					HostPath:  v,
					ReadWrite: container.VolumesRW[k],
				}
			}
			if container.Node != nil {
				runtimeContainer.Node.ID = container.Node.ID
				runtimeContainer.Node.Name = container.Node.Name
				runtimeContainer.Node.Address = Address{
					IP: container.Node.IP,
				}
			}

			for _, v := range container.Mounts {
				runtimeContainer.Mounts = append(runtimeContainer.Mounts, Mount{
					Name:        v.Name,
					Source:      v.Source,
					Destination: v.Destination,
					Driver:      v.Driver,
					Mode:        v.Mode,
					RW:          v.RW,
				})
			}

			runtimeContainer.Env = splitKeyValueSlice(container.Config.Env)
			runtimeContainer.Labels = container.Config.Labels
			containers = append(containers, runtimeContainer)
		}
	}
	return containers, nil

}

func newSignalChannel() <-chan os.Signal {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)

	return sig
}

func newDebounceChannel(input chan *docker.APIEvents, wait *Wait) chan *docker.APIEvents {
	if wait == nil {
		return input
	}
	if wait.Min == 0 {
		return input
	}

	output := make(chan *docker.APIEvents, 100)

	go func() {
		var (
			event    *docker.APIEvents
			minTimer <-chan time.Time
			maxTimer <-chan time.Time
		)

		defer close(output)

		for {
			select {
			case buffer, ok := <-input:
				if !ok {
					return
				}
				event = buffer
				minTimer = time.After(wait.Min)
				if maxTimer == nil {
					maxTimer = time.After(wait.Max)
				}
			case <-minTimer:
				log.Println("Debounce minTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			case <-maxTimer:
				log.Println("Debounce maxTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			}
		}
	}()

	return output
}
