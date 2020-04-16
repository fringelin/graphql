package graphql

import (
	context2 "context"
	"encoding/json"
	"errors"
	"fmt"
	errors2 "github.com/shyptr/graphql/errors"
	"github.com/shyptr/graphql/execution"
	"github.com/shyptr/graphql/internal"
	"github.com/shyptr/graphql/schemabuilder"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"gocloud.dev/pubsub"
)

// HTTPSubHandler implements the handler required for executing the graphql subscriptions
func HTTPSubHandler(schema *internal.Schema, s *pubsub.Subscription) (http.Handler, func()) {
	source := make(chan *event)
	sessions := &sessions{
		data:  map[string][]chan *event{},
		chans: map[string][]chan struct{}{},
	}
	return &httpSubHandler{
			Handler: Handler{
				Schema:   schema,
				Executor: &execution.Executor{},
			},
			qmHandler: HTTPHandler(schema),
			upgrader:  &websocket.Upgrader{},
			source:    source,
			sessions:  sessions,
		}, func() {
			go startListening(s, source, func() {
				exit(sessions)
			})
			go listenSource(source, sessions)
		}
}

func listenSource(events chan *event, ss *sessions) {
	for evt := range events {
		ss.RLock()
		for _, v := range ss.data {
			for _, s := range v {
				s <- evt
			}
		}
		ss.RUnlock()
	}
}

func startListening(s *pubsub.Subscription, source chan<- *event, cancel func()) {
	for {
		msg, err := s.Receive(context2.Background())
		if err != nil {
			fmt.Println("Pubsub failed: ", err)
			cancel()
			return
		}
		msg.Ack()

		source <- &event{
			payload: msg.Body,
			typ:     msg.Metadata["type"],
		}
	}
}

type httpSubHandler struct {
	Handler
	qmHandler http.Handler
	upgrader  *websocket.Upgrader
	source    chan *event
	sessions  *sessions
}

type event struct {
	typ     string
	payload []byte
}

type sessions struct {
	sync.RWMutex
	data  map[string][]chan *event
	chans map[string][]chan struct{}
}

type wsMessage struct {
	Type    string          `json:"type"`
	Id      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type gqlPayload struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
	OpName    string                 `json:"operationName"`
}

func (h *httpSubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet { // If not a subscription request route to normal handler
		h.qmHandler.ServeHTTP(w, r)
		return
	}
	log.Println("Request Headers:", r.Header)

	// Check origin and set response headers
	h.upgrader.CheckOrigin = func(r *http.Request) bool { return true }
	res := http.Header{}
	res["Sec-Websocket-Protocol"] = []string{"graphql-ws"}

	con, err := h.upgrader.Upgrade(w, r, res)
	if err != nil {
		fmt.Println("failed to upgrade to websocket:", err)
		return
	}
	defer con.Close()

	if con.Subprotocol() != "graphql-ws" {
		fmt.Println("invalid subprotocol")
		return
	}

	var msg wsMessage

	if err := con.ReadJSON(&msg); err != nil {
		fmt.Println("failed to parse websocket message: ", err)
		return
	}
	conn := &webConn{conn: con}
	if msg.Type != "connection_init" {
		if err := writeResponse(conn, "connection_error", "", nil, errors.New("expected init message")); err != nil {
			fmt.Println(err)
			return
		}
	}
	if err := writeResponse(conn, "connection_ack", "", nil, nil); err != nil {
		fmt.Println(err)
		return
	}
loop:
	for {
		var data wsMessage
		if err := con.ReadJSON(&data); err != nil {
			if err := writeResponse(conn, "connection_error", "", nil, err); err != nil {
				fmt.Println(err)
				return
			}
			fmt.Println(err)
		}
		switch data.Type {
		case "start":
			var gql gqlPayload
			if err := json.Unmarshal(data.Payload, &gql); err != nil {
				if err := writeResponse(conn, "connection_error", "", nil, err); err != nil {
					fmt.Println(err)
					return
				}
				fmt.Println(err)
				return
			}
			query, err := internal.Parse(gql.Query)
			if err != nil {
				if er := writeResponse(conn, "error", data.Id, nil, err); er != nil {
					fmt.Println(err)
					return
				}
				fmt.Println(err)
				return
			}
			schema := h.Schema.Subscription
			//if err := validation.Validate(h.Schema, query, gql.Variables, 50); err != nil {
			//	if er := writeResponse(conn, "error", data.Id, nil, err); er != nil {
			//		fmt.Println(er)
			//		return
			//	}
			//	fmt.Println(err)
			//	return
			//}
			_, selectionSet, err := execution.ApplySelectionSet(h.Schema, query, "subscription", gql.Variables)
			if err != nil {
				if er := writeResponse(conn, "error", data.Id, nil, err); er != nil {
					fmt.Println(er)
					return
				}
				fmt.Println(err)
				return
			}
			for _, v := range selectionSet.Selections {
				end := make(chan struct{}, 1)
				modQuery := &internal.SelectionSet{
					Selections: []*internal.Selection{v},
					Fragments:  selectionSet.Fragments,
				}
				go func(conn *webConn, data *wsMessage, schema internal.Type, query *internal.SelectionSet, end chan struct{}, w http.ResponseWriter, r *http.Request) {
					if err := h.serveHTTP(conn, *data, schema, query, end, w, r); err != nil {
						fmt.Println("Id:", data.Id, ": terminated: ", err)
					}
					h.sessions.Lock()
					if _, ok := h.sessions.data[data.Id]; ok {
						if err := writeResponse(conn, "complete", data.Id, nil, nil); err != nil {
							fmt.Println(err)
						}
						fmt.Println("Id:", data.Id, ": terminated.")
					}
					delete(h.sessions.data, data.Id)
					delete(h.sessions.chans, data.Id)
					h.sessions.Unlock()
				}(conn, &data, schema, modQuery, end, w, r)
			}
		case "stop":
			h.sessions.RLock()
			for _, v := range h.sessions.chans[data.Id] {
				v <- struct{}{}
			}
			h.sessions.RUnlock()
		case "connection_terminate":
			exit(h.sessions)
			break loop
		default:
		}
	}
}

func exit(ss *sessions) {
	ss.RLock()
	for _, v := range ss.chans {
		for _, s := range v {
			s <- struct{}{}
		}
	}
	for _, v := range ss.data {
		for _, s := range v {
			close(s)
		}
	}
	ss.RUnlock()
}

type webConn struct {
	sync.Mutex
	conn *websocket.Conn
}

func writeResponse(w *webConn, typ, id string, r interface{}, er error) error {
	var payload []byte
	var err error
	if typ == "data" {
		if er != nil {
			payload, err = json.Marshal(Response{Data: r, Errors: errors2.MultiError{errors2.New(er.Error())}})
			if err != nil {
				return err
			}
		} else {
			payload, err = json.Marshal(Response{Data: r, Errors: errors2.MultiError{}})
			if err != nil {
				return err
			}
		}
	} else if typ == "error" || typ == "connection_error" {
		str := strings.Replace(er.Error(), "\"", "\\\"", -1)
		payload = json.RawMessage("{ \"error\" : \"" + str + "\"}")
	}
	res := wsMessage{
		Type:    typ,
		Id:      id,
		Payload: payload,
	}
	w.Lock()
	if err := w.conn.WriteJSON(res); err != nil {
		w.Unlock()
		return err
	}
	w.Unlock()
	return nil
}

func (h *httpSubHandler) serveHTTP(conn *webConn, data wsMessage, schema internal.Type, query *internal.SelectionSet, end chan struct{}, w http.ResponseWriter, r *http.Request) error {
	sid := data.Id
	sess := make(chan *event)
	h.sessions.Lock()
	h.sessions.data[sid] = append(h.sessions.data[sid], sess)
	h.sessions.chans[sid] = append(h.sessions.chans[sid], end)
	h.sessions.Unlock()

	cls := func(ss *sessions, sid string) {
		ss.Lock()
		for _, v := range ss.data[sid] {
			close(v)
			for range v {
			}
		}
		for _, v := range ss.chans[sid] {
			close(v)
			for range v {
			}
		}
		ss.Unlock()
	}

	// Listening on usrChannel for any source event of subType
	for msg := range sess {
		select {
		case <-end:
			cls(h.sessions, sid)
			return nil
		default:
			if err := func() error {
				res, err := h.Executor.Execute(r.Context(), schema, &schemabuilder.Subscription{msg.payload}, query)
				rer := err
				if err := writeResponse(conn, "data", data.Id, res, rer); err != nil {
					return err
				}
				if rer != nil {
					return err
				}
				return nil
			}(); err != nil {
				cls(h.sessions, sid)
				return err
			}
		}
	}
	return nil
}
