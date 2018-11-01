package epsagon

import (
	"bytes"
	"fmt"
	protocol "github.com/epsagon/epsagon-go/protocol"
	"github.com/golang/protobuf/jsonpb"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

var (
	mutex        sync.Mutex
	globalTracer tracer
)

type tracer interface {
	AddEvent(*protocol.Event)
	AddException(*protocol.Exception)
	Run()
	Running() bool
	Stop()
	Stopped() bool
}

// Config is the configuration for Epsagon's tracer
type Config struct {
	ApplicationName string
	Token           string
	CollectorURL    string
	MetadataOnly    bool
	Debug           bool
}

type epsagonTracer struct {
	Config *Config

	eventsPipe     chan *protocol.Event
	events         []*protocol.Event
	exceptionsPipe chan *protocol.Exception
	exceptions     []*protocol.Exception

	closeCmd chan struct{}
	stopped  chan struct{}
	running  chan struct{}
}

func (tracer *epsagonTracer) sendTraces() {
	tracesReader, err := tracer.getTraceReader()
	if err != nil {
		// TODO create an exception and send a trace only with that
		log.Printf("Epsagon: Encountered an error while marshaling the traces: %v\n", err)
		return
	}
	client := &http.Client{Timeout: time.Duration(time.Second)}

	resp, err := client.Post(tracer.Config.CollectorURL, "application/json", tracesReader)
	if err != nil {
		var respBody []byte
		resp.Body.Read(respBody)
		resp.Body.Close()
		log.Printf("Error while sending traces \n%v\n%v\n", err, respBody)
	}
}

func (tracer *epsagonTracer) getTraceReader() (io.Reader, error) {
	version := runtime.Version()
	trace := protocol.Trace{
		AppName:    tracer.Config.ApplicationName,
		Token:      tracer.Config.Token,
		Events:     tracer.events,
		Exceptions: tracer.exceptions,
		Version:    "0.0.1",
		Platform:   version,
	}
	marshaler := jsonpb.Marshaler{
		EnumsAsInts: true, EmitDefaults: true, OrigName: true}
	traceJSON, err := marshaler.MarshalToString(&trace)
	if err != nil {
		return nil, err
	}
	if tracer.Config.Debug {
		log.Printf("Final Traces: %s ", traceJSON)
	}
	return bytes.NewBuffer([]byte(traceJSON)), nil
}

func isChannelPinged(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Running return true iff the tracer has been running
func (tracer *epsagonTracer) Running() bool {
	return isChannelPinged(tracer.running)
}

// Stopped return true iff the tracer has been closed
func (tracer *epsagonTracer) Stopped() bool {
	return isChannelPinged(tracer.stopped)
}

func fillConfigDefaults(config *Config) {
	if !config.Debug {
		if os.Getenv("EPSAGON_DEBUG") == "TRUE" {
			config.Debug = true
		}
	}
	if len(config.Token) == 0 {
		config.Token = os.Getenv("EPSAGON_TOKEN")
		if config.Debug {
			log.Println("EPSAGON DEBUG: setting token from environment variable")
		}
	}
	if len(config.CollectorURL) == 0 {
		region := os.Getenv("AWS_REGION")
		if len(region) != 0 {
			config.CollectorURL = fmt.Sprintf("http://%s.tc.epsagon.com", region)
		} else {
			config.CollectorURL = "http://us-east-1.tc.epsagon.com"
		}
		if config.Debug {
			log.Printf("EPSAGON DEBUG: setting collector url to %s", config.CollectorURL)
		}
	}
}

// CreateTracer will initiallize a global epsagon tracer
func CreateTracer(config *Config) {
	mutex.Lock()
	defer mutex.Unlock()
	if globalTracer != nil && !globalTracer.Stopped() {
		log.Println("The tracer is already created")
		return
	}
	fillConfigDefaults(config)
	globalTracer = &epsagonTracer{
		Config:         config,
		eventsPipe:     make(chan *protocol.Event),
		events:         make([]*protocol.Event, 0, 0),
		exceptionsPipe: make(chan *protocol.Exception),
		exceptions:     make([]*protocol.Exception, 0, 0),
		closeCmd:       make(chan struct{}),
		stopped:        make(chan struct{}),
		running:        make(chan struct{}),
	}
	if config.Debug {
		log.Println("EPSAGON DEBUG: Created a new tracer")
	}
	go globalTracer.Run()
}

// AddException adds a tracing exception to the tracer
func (tracer *epsagonTracer) AddException(exception *protocol.Exception) {
	tracer.exceptionsPipe <- exception
}

// AddEvent adds an event to the tracer
func (tracer *epsagonTracer) AddEvent(event *protocol.Event) {
	tracer.eventsPipe <- event
	if tracer.Config.Debug {
		log.Println("EPSAGON DEBUG: Adding event: ", event)
	}
}

// AddEvent adds an event to the tracer
func AddEvent(event *protocol.Event) {
	if globalTracer == nil || globalTracer.Stopped() {
		// TODO
		log.Println("The tracer is not initialized!")
		return
	}
	globalTracer.AddEvent(event)
}

// AddException adds an exception to the tracer
func AddException(exception *protocol.Exception) {
	if globalTracer == nil || globalTracer.Stopped() {
		// TODO
		log.Println("The tracer is not initialized!")
		return
	}
	globalTracer.AddException(exception)
}

// Stop stops the tracer running routine
func (tracer *epsagonTracer) Stop() {
	select {
	case <-tracer.stopped:
		return
	default:
		tracer.closeCmd <- struct{}{}
		<-tracer.stopped
	}
}

// StopTracer will close the tracer and send all the data to the collector
func StopTracer() {
	if globalTracer == nil || globalTracer.Stopped() {
		// TODO
		log.Println("The tracer is not initialized!")
		return
	}
	globalTracer.Stop()
}

// Run starts the runner background routine that will
// run until it
func (tracer *epsagonTracer) Run() {
	if tracer.Config.Debug {
		log.Println("EPSAGON DEBUG: tracer started running")
	}
	if tracer.Running() {
		return
	}
	close(tracer.running)
	defer func() { tracer.running = make(chan struct{}) }()
	defer close(tracer.stopped)

	for {
		select {
		case event := <-tracer.eventsPipe:
			tracer.events = append(tracer.events, event)
		case exception := <-tracer.exceptionsPipe:
			tracer.exceptions = append(tracer.exceptions, exception)
		case <-tracer.closeCmd:
			if tracer.Config.Debug {
				log.Println("EPSAGON DEBUG: tracer stops running, sending traces")
			}
			tracer.sendTraces()
			return
		}
	}
}
