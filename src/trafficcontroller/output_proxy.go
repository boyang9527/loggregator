package trafficcontroller

import (
	"code.google.com/p/gogoprotobuf/proto"
	"fmt"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	"github.com/gorilla/websocket"
	"net/http"
	"net/url"
	"time"
	"trafficcontroller/authorization"
	"trafficcontroller/hasher"
)

type Proxy struct {
	host      string
	hashers   []*hasher.Hasher
	logger    *gosteno.Logger
	authorize authorization.LogAccessAuthorizer
}

func NewProxy(host string, hashers []*hasher.Hasher, authorizer authorization.LogAccessAuthorizer, logger *gosteno.Logger) *Proxy {
	return &Proxy{host: host, hashers: hashers, authorize: authorizer, logger: logger}
}

func (proxy *Proxy) Start() error {
	return http.ListenAndServe(proxy.host, proxy)
}

func (proxy *Proxy) isAuthorized(appId, authToken string, clientAddress string) (bool, *logmessage.LogMessage) {
	newLogMessage := func(message []byte) *logmessage.LogMessage {
		currentTime := time.Now()
		messageType := logmessage.LogMessage_ERR

		return &logmessage.LogMessage{
			Message:     message,
			AppId:       proto.String(appId),
			MessageType: &messageType,
			SourceName:  proto.String("LGR"),
			Timestamp:   proto.Int64(currentTime.UnixNano()),
		}
	}

	if appId == "" {
		message := fmt.Sprintf("HttpServer: Did not accept sink connection with invalid app id: %s.", clientAddress)
		proxy.logger.Warn(message)
		return false, newLogMessage([]byte("Error: Invalid target"))
	}

	if authToken == "" {
		message := fmt.Sprintf("HttpServer: Did not accept sink connection from %s without authorization.", clientAddress)
		proxy.logger.Warnf(message)
		return false, newLogMessage([]byte("Error: Authorization not provided"))
	}

	if !proxy.authorize(authToken, appId, proxy.logger) {
		message := fmt.Sprintf("HttpServer: Auth token [%s] not authorized to access appId [%s].", authToken, appId)
		proxy.logger.Warn(message)
		return false, newLogMessage([]byte("Error: Invalid authorization"))
	}

	return true, nil
}

func upgrade(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	ws, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if err != nil {
		http.Error(w, "Not a websocket handshake", 400)
		return nil
	}
	return ws
}

func (proxy *Proxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	r.Form.Get("app")
	clientAddress := r.RemoteAddr
	requestUri := r.URL.RequestURI()
	appId := r.Form.Get("app")

	extractAuthTokenFromUrl := func(u *url.URL) string {
		authorization := ""
		queryValues := u.Query()
		if len(queryValues["authorization"]) == 1 {
			authorization = queryValues["authorization"][0]
		}
		return authorization
	}

	authToken := r.Header.Get("Authorization")
	if authToken == "" {
		authToken = extractAuthTokenFromUrl(r.URL)
	}

	ws := upgrade(rw, r)
	if authorized, errorMessage := proxy.isAuthorized(appId, authToken, clientAddress); !authorized {
		data, err := proto.Marshal(errorMessage)
		if err != nil {
			proxy.logger.Errorf("Error marshalling log message: %s", err)
		}
		ws.WriteMessage(websocket.BinaryMessage, data)

		ws.Close()
		return
	}

	proxy.HandleWebSocket(ws, appId, requestUri)
}

func (proxy *Proxy) HandleWebSocket(clientWS *websocket.Conn, appId, requestUri string) {
	defer clientWS.Close()

	proxy.logger.Debugf("Output Proxy: Request for app: %v", appId)
	serverWSs := make([]*websocket.Conn, len(proxy.hashers))
	for index, hasher := range proxy.hashers {
		proxy.logger.Debugf("Output Proxy: Servers in group [%v]: %v", index, hasher.LoggregatorServers())

		server := hasher.GetLoggregatorServerForAppId(appId)
		proxy.logger.Debugf("Output Proxy: AppId is %v. Using server: %v", appId, server)

		serverWS, _, err := websocket.DefaultDialer.Dial("ws://"+server+requestUri, http.Header{})

		if err != nil {
			proxy.logger.Errorf("Output Proxy: Error connecting to loggregator server - %v", err)
		}

		if serverWS != nil {
			serverWSs[index] = serverWS
		}
	}
	proxy.forwardIO(serverWSs, clientWS)

}

func (proxy *Proxy) proxyConnectionTo(server *websocket.Conn, client *websocket.Conn, doneChan chan bool) {
	proxy.logger.Debugf("Output Proxy: Starting to listen to server %v", server.RemoteAddr().String())

	var logMessage []byte
	defer server.Close()
	for {

		_, data, err := server.ReadMessage()

		if err != nil {
			proxy.logger.Errorf("Output Proxy: Error reading from the server - %v - %v", err, server.RemoteAddr().String())
			doneChan <- true
			return
		}
		proxy.logger.Debugf("Output Proxy: Got message from server %v bytes", len(logMessage))

		err = client.WriteMessage(websocket.BinaryMessage, data)
		if err != nil {
			proxy.logger.Errorf("Output Proxy: Error writing to client websocket - %v", err)
			return
		}
	}
}

func (proxy *Proxy) watchKeepAlive(servers []*websocket.Conn, client *websocket.Conn) {
	for {
		_, keepAlive, err := client.ReadMessage()
		if err != nil {
			proxy.logger.Errorf("Output Proxy: Error reading from the client - %v", err)
			return
		}
		proxy.logger.Debugf("Output Proxy: Got message from client %v bytes", len(keepAlive))
		for _, server := range servers {
			server.WriteMessage(websocket.BinaryMessage, keepAlive)
		}
	}
}

func (proxy *Proxy) forwardIO(servers []*websocket.Conn, client *websocket.Conn) {
	doneChan := make(chan bool)

	for _, server := range servers {
		go proxy.proxyConnectionTo(server, client, doneChan)
	}

	go proxy.watchKeepAlive(servers, client)

	for _, server := range servers {
		<-doneChan
		proxy.logger.Debugf("Output Proxy: Lost one server %s", server.RemoteAddr().String())
	}
	proxy.logger.Debugf("Output Proxy: Terminating connection. All clients disconnected")
}
