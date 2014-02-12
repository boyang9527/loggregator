package sinks

import (
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/cfcomponent/instrumentation"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	"github.com/gorilla/websocket"
	"net"
	"sync/atomic"
	"time"
)

type WebsocketSink struct {
	logger              *gosteno.Logger
	appId               string
	ws                  *websocket.Conn
	clientAddress       net.Addr
	sentMessageCount    uint64
	sentByteCount       uint64
	keepAliveInterval   time.Duration
	sinkCloseChan       chan Sink
	wsMessageBufferSize uint
}

func NewWebsocketSink(appId string, givenLogger *gosteno.Logger, ws *websocket.Conn, sinkCloseChan chan Sink, keepAliveInterval time.Duration, wsMessageBufferSize uint) Sink {
	return &WebsocketSink{
		logger:              givenLogger,
		appId:               appId,
		ws:                  ws,
		clientAddress:       ws.RemoteAddr(),
		keepAliveInterval:   keepAliveInterval,
		sinkCloseChan:       sinkCloseChan,
		wsMessageBufferSize: wsMessageBufferSize,
	}
}

func (sink *WebsocketSink) keepAliveFailureChannel() <-chan bool {
	keepAliveFailureChan := make(chan bool)
	keepAliveChan := make(chan bool)
	go func() {
		for {
			_, _, err := sink.ws.ReadMessage()
			if err != nil {
				sink.logger.Debugf("Websocket Sink %s: Error receiving keep-alive. Stopping listening. Err: %v", sink.clientAddress, err)
				return
			}
			keepAliveChan <- true
		}
	}()

	go func() {
		for {
			select {
			case <-keepAliveChan:
				sink.logger.Debugf("Websocket Sink %s: Keep-alive received", sink.clientAddress)
			case <-time.After(sink.keepAliveInterval):
				keepAliveFailureChan <- true
				return
			}
		}
	}()
	return keepAliveFailureChan
}

func (sink *WebsocketSink) Identifier() string {
	return sink.ws.RemoteAddr().String()
}

func (sink *WebsocketSink) AppId() string {
	return sink.appId
}

func (sink *WebsocketSink) ShouldReceiveErrors() bool {
	return true
}


func (sink *WebsocketSink) Run(inputChan <-chan *logmessage.Message) {
	sink.logger.Debugf("Websocket Sink %s: Created for appId [%s]", sink.clientAddress, sink.appId)

	keepAliveFailure := sink.keepAliveFailureChannel()
	alreadyRequestedClose := false

	buffer := RunTruncatingBuffer(inputChan, sink.wsMessageBufferSize, sink.logger)
	for {
		sink.logger.Debugf("Websocket Sink %s: Waiting for activity", sink.clientAddress)
		select {
		case <-keepAliveFailure:
			sink.ws.Close()
			sink.logger.Debugf("Websocket Sink %s: No keep keep-alive received. Requesting close.", sink.clientAddress)
			sink.requestClose(sink.sinkCloseChan, &alreadyRequestedClose)
			return
		case message, ok := <-buffer.GetOutputChannel():
			if !ok {
				sink.logger.Debugf("Websocket Sink %s: Closed listener channel detected. Closing websocket", sink.clientAddress)
				sink.ws.Close()
				sink.logger.Debugf("Websocket Sink %s: Websocket successfully closed", sink.clientAddress)
				return
			}
			sink.logger.Debugf("Websocket Sink %s: Got %d bytes. Sending data", sink.clientAddress, message.GetRawMessageLength())
			err := sink.ws.WriteMessage(websocket.BinaryMessage, message.GetRawMessage())
			if err != nil {
				sink.logger.Debugf("Websocket Sink %s: Error when trying to send data to sink %s. Requesting close. Err: %v", sink.clientAddress, err)
				sink.requestClose(sink.sinkCloseChan, &alreadyRequestedClose)
			} else {
				sink.logger.Debugf("Websocket Sink %s: Successfully sent data", sink.clientAddress)
				atomic.AddUint64(&sink.sentMessageCount, 1)
				atomic.AddUint64(&sink.sentByteCount, uint64(message.GetRawMessageLength()))
			}
		}
	}
}

func (sink *WebsocketSink) Emit() instrumentation.Context {
	return instrumentation.Context{Name: "websocketSink",
		Metrics: []instrumentation.Metric{
			instrumentation.Metric{Name: "sentMessageCount:" + sink.appId, Value: atomic.LoadUint64(&sink.sentMessageCount)},
			instrumentation.Metric{Name: "sentByteCount:" + sink.appId, Value: atomic.LoadUint64(&sink.sentByteCount)},
		},
	}
}

func (sink *WebsocketSink) requestClose(sinkCloseChan chan Sink, alreadyRequestedClose *bool) {
	if !(*alreadyRequestedClose) {
		sinkCloseChan <- sink
		*alreadyRequestedClose = true
		sink.logger.Debugf("Sink for App %s: Successfully requested listener channel close", sink.AppId())
	} else {
		sink.logger.Debugf("Sink for App %s: Previously requested close. Doing nothing", sink.AppId())
	}
}
