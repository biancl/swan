package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/src/config"
	eventbus "github.com/Dataman-Cloud/swan/src/event"
	"github.com/Dataman-Cloud/swan/src/janitor"
	"github.com/Dataman-Cloud/swan/src/janitor/upstream"
	"github.com/Dataman-Cloud/swan/src/nameserver"
	"github.com/Dataman-Cloud/swan/src/types"
	"github.com/Dataman-Cloud/swan/src/utils"
	"github.com/Dataman-Cloud/swan/src/utils/httpclient"

	"github.com/Sirupsen/logrus"
	"golang.org/x/net/context"
)

const REJOIN_BACKOFF = 3 * time.Second
const SSE_DATA_PREFIX = "data:"
const SSE_EVENT_PREFIX = "event:"
const SSE_BLANK_LINE = ""

// Agent struct
type Agent struct {
	Resolver   *nameserver.Resolver
	Janitor    *janitor.JanitorServer
	HTTPServer *HTTPServer
	Config     config.AgentConfig
	eventCh    chan *event
}

type event struct {
	name    string
	payload []byte
}

// New agent func
func New(agentConf config.AgentConfig) *Agent {
	agent := &Agent{
		Config:   agentConf,
		Resolver: nameserver.NewResolver(&agentConf.DNS),
		Janitor:  janitor.NewJanitorServer(&agentConf.Janitor),
		eventCh:  make(chan *event, 1024),
	}
	agent.HTTPServer = NewHTTPServer(agentConf.ListenAddr, agent)
	return agent
}

// StartAndJoin func
func (agent *Agent) StartAndJoin() error {
	errCh := make(chan error)

	go func() {
		err := agent.Resolver.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("resolver quit, error:", err)
	}()

	go func() {
		err := agent.Janitor.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("janitor quit, error:", err)
	}()

	go func() {
		err := agent.HTTPServer.Start()
		if err != nil {
			errCh <- err
		}
		logrus.Warnln("http server quit, error:", err)
	}()

	go agent.watchEvents()
	go agent.dispatchEvents()

	return <-errCh
}

// watchEvents establish a connection to swan master's stream events endpoint
// and broadcast received events
func (agent *Agent) watchEvents() {
	for {
		leaderAddr, err := agent.detectManagerLeader()
		if err != nil {
			logrus.Errorf("detect manager leader got error: %v, retry ...", err)
			time.Sleep(REJOIN_BACKOFF)
			continue
		}
		logrus.Printf("detected manager addr %s, listening on events ...", leaderAddr)

		err = agent.watchManagerEvents(leaderAddr)
		if err != nil {
			logrus.Errorf("watch manager events got error: %v, retry ...", err)
			time.Sleep(REJOIN_BACKOFF)
		}
	}
}

// dispatchEvents dispatch received events to dns & proxy goroutines
func (agent *Agent) dispatchEvents() {
	// send proxy info to dns proxy listener
	agent.Resolver.EmitChange(&nameserver.RecordChangeEvent{
		Change: "add",
		Record: nameserver.Record{
			Type:    nameserver.A,
			Ip:      agent.Config.Janitor.AdvertiseIP,
			IsProxy: true,
		},
	})

	for event := range agent.eventCh {
		var taskInfoEvent types.TaskInfoEvent
		err := json.Unmarshal(event.payload, &taskInfoEvent)
		if err != nil {
			logrus.Errorf("unmarshal taskInfoEvent go error: %s", err.Error())
			continue
		}

		if taskInfoEvent.GatewayEnabled {
			agent.Janitor.EmitEvent(genJanitorBackendEvent(
				event.name, &taskInfoEvent))
		}

		// Resolver only recongnize these two events
		if event.name == eventbus.EventTypeTaskHealthy ||
			event.name == eventbus.EventTypeTaskUnhealthy {
			agent.Resolver.EmitChange(recordChangeEventFromTaskInfoEvent(
				event.name, &taskInfoEvent))
		}
	}
}

// todo
func (agent *Agent) detectManagerLeader() (leaderAddr string, err error) {
	for _, managerAddr := range agent.Config.JoinAddrs {
		nodeRegistrationPath := managerAddr + "/ping"
		_, err := httpclient.NewDefaultClient().GET(context.TODO(), nodeRegistrationPath, nil, nil)
		if err != nil {
			logrus.Infof("register to %s got error: %s", nodeRegistrationPath, err.Error())
			continue
		}

		return managerAddr, nil
	}

	return "", errors.New("try join all managers are failed")
}

func (agent *Agent) watchManagerEvents(leaderAddr string) error {
	eventsDoesMatter := []string{
		eventbus.EventTypeTaskUnhealthy,
		eventbus.EventTypeTaskHealthy,
		eventbus.EventTypeTaskWeightChange,
	}

	eventsPath := fmt.Sprintf("http://%s/events?catchUp=true", leaderAddr)
	resp, err := http.Get(eventsPath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		// skip blank line
		if line == SSE_BLANK_LINE {
			continue
		}

		if strings.HasPrefix(line, SSE_EVENT_PREFIX) {
			eventType := strings.TrimSpace(line[len(SSE_EVENT_PREFIX):len(line)])
			if !utils.SliceContains(eventsDoesMatter, eventType) {
				continue
			}

			// read next line of stream
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			// if line is not data section
			if !strings.HasPrefix(line, SSE_DATA_PREFIX) {
				continue
			}

			agent.eventCh <- &event{
				name:    eventType,
				payload: []byte(line[len(SSE_DATA_PREFIX):len(line)]),
			}
		}
	}
}

func recordChangeEventFromTaskInfoEvent(eventType string, taskInfoEvent *types.TaskEvent) *nameserver.RecordChangeEvent {
	resolverEvent := &nameserver.RecordChangeEvent{}
	if eventType == eventbus.EventTypeTaskHealthy {
		resolverEvent.Change = "add"
	} else {
		resolverEvent.Change = "del"
	}

	// port & type
	if taskInfoEvent.Mode == "replicates" {
		resolverEvent.Type = nameserver.SRV ^ nameserver.A
		resolverEvent.Port = fmt.Sprintf("%d", taskInfoEvent.Port)
	} else {
		resolverEvent.Type = nameserver.A
	}
	// the rest
	resolverEvent.Cluster = taskInfoEvent.ClusterID
	resolverEvent.RunAs = taskInfoEvent.RunAs
	resolverEvent.AppName = taskInfoEvent.AppName
	resolverEvent.InsName = taskInfoEvent.InsName
	resolverEvent.SlotID = fmt.Sprintf("%d", taskInfoEvent.SlotIndex)
	resolverEvent.Ip = taskInfoEvent.IP
	resolverEvent.Weight = taskInfoEvent.Weight

	return resolverEvent
}

func genJanitorBackendEvent(eventType string, taskInfoEvent *types.TaskInfoEvent) *upstream.BackendEvent {
	var (
		act string

		// upstream
		ups    = taskInfoEvent.AppID
		alias  = "" // TODO
		listen = "" // TODO

		// backend
		backend = taskInfoEvent.TaskID
		ip      = taskInfoEvent.IP
		port    = taskInfoEvent.Port
		weight  = taskInfoEvent.Weight
		version = taskInfoEvent.VersionID
	)

	switch eventType {
	case eventbus.EventTypeTaskHealthy:
		act = "add"
	case eventbus.EventTypeTaskUnhealthy:
		act = "del"
	case eventbus.EventTypeTaskWeightChange:
		act = "change"
	default:
		return nil
	}

	return upstream.BuildBackendEvent(act, ups, alias, listen, backend, ip, version, port, weight)
}