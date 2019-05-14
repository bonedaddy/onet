package onet

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"reflect"
	"regexp"
	"strings"

	"go.dedis.ch/onet/v3/log"
	"go.dedis.ch/onet/v3/network"
	"go.dedis.ch/protobuf"
)

// ServiceProcessor allows for an easy integration of external messages
// into the Services. You have to embed it into your Service-struct as
// a pointer. It will process client requests that have been registered
// with RegisterMessage.
type ServiceProcessor struct {
	handlers map[string]serviceHandler
	*Context
}

// serviceHandler stores the handler and the message-type.
type serviceHandler struct {
	handler   interface{}
	msgType   reflect.Type
	streaming bool
}

// NewServiceProcessor initializes your ServiceProcessor.
func NewServiceProcessor(c *Context) *ServiceProcessor {
	return &ServiceProcessor{
		handlers: make(map[string]serviceHandler),
		Context:  c,
	}
}

var errType = reflect.TypeOf((*error)(nil)).Elem()

// RegisterHandler will store the given handler that will be used by the service.
// WebSocket will then forward requests to "ws://service_name/struct_name"
// to the given function f, which must be in the following form:
// func(msg interface{})(ret interface{}, err error)
//
//  * msg is a pointer to a structure to the message sent.
//  * ret is a pointer to a struct of the return-message.
//  * err is an error, it can be nil, or any type that implements error.
//
// struct_name is stripped of its package-name, so a structure like
// network.Body will be converted to Body.
func (p *ServiceProcessor) RegisterHandler(f interface{}) error {
	if err := handlerInputCheck(f); err != nil {
		return err
	}

	pm, sh, err := createServiceHandler(f)
	if err != nil {
		return err
	}
	p.handlers[pm] = sh

	return nil
}

// RegisterStreamingHandler stores a handler that is responsible for streaming
// messages to the client via a channel. Websocket will accept requests for
// this handler at "ws://service_name/struct_name", where struct_name is
// argument of f, which must be in the form:
// func(msg interface{})(retChan chan interface{}, closeChan chan bool, err error)
//
//  * msg is a pointer to a structure to the message sent.
//  * retChan is a channel of a pointer to a struct, everything sent into this
//    channel will be forwarded to the client, if there are no more messages,
//    the service should close retChan.
//  * closeChan is a boolean channel, upon receiving a message on this channel,
//    the handler must stop sending messages and close retChan.
//  * err is an error, it can be nil, or any type that implements error.
//
// struct_name is stripped of its package-name, so a structure like
// network.Body will be converted to Body.
func (p *ServiceProcessor) RegisterStreamingHandler(f interface{}) error {
	if err := handlerInputCheck(f); err != nil {
		return err
	}

	// check output
	ft := reflect.TypeOf(f)
	if ft.NumOut() != 3 {
		return errors.New("Need 3 return values: chan interface{}, chan bool and error")
	}
	// first output
	ret0 := ft.Out(0)
	if ret0.Kind() != reflect.Chan {
		return errors.New("1st return value must be a channel")
	}
	if ret0.Elem().Kind() != reflect.Interface {
		if ret0.Elem().Kind() != reflect.Ptr {
			return errors.New("1st return value must be a channel of a *pointer* to a struct")
		}
		if ret0.Elem().Elem().Kind() != reflect.Struct {
			return errors.New("1st return value must be a channel of a pointer to a *struct*")
		}
	}
	// second output
	ret1 := ft.Out(1)
	if ret1.Kind() != reflect.Chan {
		return errors.New("2nd return value must be a channel")
	}
	if ret1.Elem().Kind() != reflect.Bool {
		return errors.New("2nd return value must be a boolean channel")
	}
	// third output
	if !ft.Out(2).Implements(errType) {
		return errors.New("3rd return value has to implement error, but is: " +
			ft.Out(2).String())
	}

	cr := ft.In(0)
	log.Lvl4("Registering streaming handler", cr.String())
	pm := strings.Split(cr.Elem().String(), ".")[1]
	p.handlers[pm] = serviceHandler{f, cr.Elem(), true}

	return nil
}

// getRouter returns the gorilla mutiplexing router. If we need to support
// arbitrary registration of REST API, we could make this method public.
func (p *ServiceProcessor) getRouter() *http.ServeMux {
	return p.server.WebSocket.mux
}

type kindGET int

const (
	invalidGET kindGET = iota
	emptyGET
	intGET
	sliceGET
)

// prepareHandlerGET check whether the first argument of f has any fields; if
// it does then make sure the number of fields is either 0 or 1; if there is 1
// field then it has to be an int or a slice of bytes.
func prepareHandlerGET(f interface{}) (kindGET, string, error) {
	in0 := reflect.TypeOf(f).In(0).Elem()
	if in0.Kind() != reflect.Struct {
		return invalidGET, "", errors.New("input argument must be a struct")
	}
	if in0.NumField() == 0 {
		return emptyGET, "", nil
	} else if in0.NumField() == 1 {
		// we support int and byte slices only
		if in0.Field(0).Type.Kind() == reflect.Slice && in0.Field(0).Type.Elem().Kind() == reflect.Uint8 {
			return sliceGET, in0.Field(0).Name, nil
		} else if in0.Field(0).Type.Kind() == reflect.Int {
			return intGET, in0.Field(0).Name, nil
		}
		return invalidGET, "", errors.New("only byte slices and int are supported")
	}
	return invalidGET, "", errors.New("number of fields must be 0 or 1")
}

// RegisterRESTHandler takes an interface of type
// func(msg interface{})(ret interface{}, err error),
// where msg and ret must be pointers to structs.
// Then registers ... TODO
func (p *ServiceProcessor) RegisterRESTHandler(f interface{}, since int, namespace, method string) error {
	// TODO can we automatically figure out the service name?
	// TODO support more methods
	if method != "GET" && method != "POST" && method != "PUT" {
		return errors.New("invalid REST method")
	}
	if since < 3 {
		return errors.New("earliest supported API level must be greater or equal to 3")
	}
	resource, sh, err := createServiceHandler(f)
	if err != nil {
		return err
	}
	var k kindGET
	var fieldNameGET string
	if method == "GET" {
		k, fieldNameGET, err = prepareHandlerGET(f)
		if err != nil {
			return err
		}
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		var msgBuf []byte
		switch r.Method {
		case "GET":
			if k == emptyGET {
				msgBuf = []byte("{}")
			} else if k == intGET {
				ok, err := regexp.MatchString(fmt.Sprintf(`^/v\d/%s/%s/\d+$`, namespace, resource), r.URL.EscapedPath())
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if !ok {
					http.Error(w, "invalid path", http.StatusNotFound)
					return
				}
				_, num := path.Split(r.URL.EscapedPath())
				msgBuf = []byte(fmt.Sprintf("{\"%s\": %s}", fieldNameGET, num))
			} else if k == sliceGET {
				ok, err := regexp.MatchString(fmt.Sprintf(`^/v\d/%s/%s/[0-9a-f]+$`, namespace, resource), r.URL.EscapedPath())
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if !ok {
					http.Error(w, "invalid path", http.StatusNotFound)
					return
				}
				_, hexStr := path.Split(r.URL.EscapedPath())
				byteBuf, err := hex.DecodeString(hexStr)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				encoded := base64.StdEncoding.EncodeToString(byteBuf)
				msgBuf = []byte(fmt.Sprintf("{\"%s\": \"%s\"}", fieldNameGET, encoded))
			} else {
				http.Error(w, "invalid GET", http.StatusBadRequest)
				return
			}
		case "POST", "PUT":
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "content type needs to be application/json", http.StatusBadRequest)
				return
			}
			var err error
			msgBuf, err = ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "unsupported method: "+r.Method, http.StatusMethodNotAllowed)
			return
		}

		msg := reflect.New(sh.msgType).Interface()
		if err := json.Unmarshal(msgBuf, msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out, tun, err := callInterfaceFunc(f, msg, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if tun != nil {
			http.Error(w, "streaming requests are not supported", http.StatusBadRequest)
			return
		}
		reply, err := json.Marshal(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(reply)
	}
	finalSlash := ""
	if k == intGET || k == sliceGET {
		finalSlash = "/"
	}
	// TODO when we have version 4 or later, add a loop here to register
	// the API for versions from `since` to the latest version.
	p.getRouter().HandleFunc(fmt.Sprintf("/v%d/%s/%s", since, namespace, resource)+finalSlash, h) //.Methods(method)
	return nil
}

func createServiceHandler(f interface{}) (string, serviceHandler, error) {
	// check output
	ft := reflect.TypeOf(f)
	if ft.NumOut() != 2 {
		return "", serviceHandler{}, errors.New("Need 2 return values: network.Body and error")
	}
	// first output
	ret := ft.Out(0)
	if ret.Kind() != reflect.Interface {
		if ret.Kind() != reflect.Ptr {
			return "", serviceHandler{},
				errors.New("1st return value must be a *pointer* to a struct or an interface")
		}
		if ret.Elem().Kind() != reflect.Struct {
			return "", serviceHandler{},
				errors.New("1st return value must be a pointer to a *struct* or an interface")
		}
	}
	// second output
	if !ft.Out(1).Implements(errType) {
		return "", serviceHandler{},
			errors.New("2nd return value has to implement error, but is: " + ft.Out(1).String())
	}

	cr := ft.In(0)
	log.Lvl4("Registering handler", cr.String())
	pm := strings.Split(cr.Elem().String(), ".")[1]

	return pm, serviceHandler{f, cr.Elem(), false}, nil
}

func handlerInputCheck(f interface{}) error {
	ft := reflect.TypeOf(f)
	if ft.Kind() != reflect.Func {
		return errors.New("Input is not a function")
	}
	if ft.NumIn() != 1 {
		return errors.New("Need one argument: *struct")
	}
	cr := ft.In(0)
	if cr.Kind() != reflect.Ptr {
		return errors.New("Argument must be a *pointer* to a struct")
	}
	if cr.Elem().Kind() != reflect.Struct {
		return errors.New("Argument must be a pointer to *struct*")
	}
	return nil
}

// RegisterHandlers takes a vararg of messages to register and returns
// the first error encountered or nil if everything was OK.
func (p *ServiceProcessor) RegisterHandlers(procs ...interface{}) error {
	for _, pr := range procs {
		if err := p.RegisterHandler(pr); err != nil {
			return err
		}
	}
	return nil
}

// RegisterStreamingHandlers takes a vararg of messages to register and returns
// the first error encountered or nil if everything was OK.
func (p *ServiceProcessor) RegisterStreamingHandlers(procs ...interface{}) error {
	for _, pr := range procs {
		if err := p.RegisterStreamingHandler(pr); err != nil {
			return err
		}
	}
	return nil
}

// Process implements the Processor interface and dispatches ClientRequest
// messages.
func (p *ServiceProcessor) Process(env *network.Envelope) {
	log.Panic("Cannot handle message.")
}

// NewProtocol is a stub for services that don't want to intervene in the
// protocol-handling.
func (p *ServiceProcessor) NewProtocol(tn *TreeNodeInstance, conf *GenericConfig) (ProtocolInstance, error) {
	return nil, nil
}

// StreamingTunnel is used as a tunnel between service processor and its
// caller, usually the websocket read-loop. When the tunnel is returned to the
// websocket loop, it should read from the out channel and forward the content
// to the client. If the client is disconnected, then the close channel should
// be closed. The signal exists to notify the service to stop streaming.
type StreamingTunnel struct {
	out   chan []byte
	close chan bool
}

func callInterfaceFunc(handler, input interface{}, streaming bool) (interface{}, chan bool, error) {
	to := reflect.TypeOf(handler).In(0)
	f := reflect.ValueOf(handler)

	arg := reflect.New(to.Elem())
	arg.Elem().Set(reflect.ValueOf(input).Elem())
	ret := f.Call([]reflect.Value{arg})

	if streaming {
		ierr := ret[2].Interface()
		if ierr != nil {
			return nil, nil, ierr.(error)
		}
		return ret[0].Interface(), ret[1].Interface().(chan bool), nil
	}
	ierr := ret[1].Interface()
	if ierr != nil {
		return nil, nil, ierr.(error)
	}
	return ret[0].Interface(), nil, nil
}

// ProcessClientRequest implements the Service interface, see the interface
// documentation.
func (p *ServiceProcessor) ProcessClientRequest(req *http.Request, path string, buf []byte) ([]byte, *StreamingTunnel, error) {
	mh, ok := p.handlers[path]
	reply, stopServiceChan, err := func() (interface{}, chan bool, error) {
		if !ok {
			err := errors.New("The requested message hasn't been registered: " + path)
			log.Error(err)
			return nil, nil, err
		}
		msg := reflect.New(mh.msgType).Interface()
		if err := protobuf.DecodeWithConstructors(buf, msg,
			network.DefaultConstructors(p.Context.server.Suite())); err != nil {
			return nil, nil, err
		}
		return callInterfaceFunc(mh.handler, msg, mh.streaming)
	}()
	if err != nil {
		return nil, nil, err
	}

	if mh.streaming {
		// We need some buffer space for the intermediate channel that
		// is responsible for forwarding messages from the service to
		// the client because we need to keep the select-loop running
		// to handle channel closures.
		outChan := make(chan []byte, 100)
		go func() {
			inChan := reflect.ValueOf(reply)
			cases := []reflect.SelectCase{
				reflect.SelectCase{Dir: reflect.SelectRecv, Chan: inChan},
			}
			for {
				chosen, v, ok := reflect.Select(cases)
				if !ok {
					log.Lvlf4("publisher is closed for %s, closing outgoing channel", path)
					close(outChan)
					return
				}
				if chosen == 0 {
					// Send information down to the client.
					buf, err = protobuf.Encode(v.Interface())
					if err != nil {
						log.Error(err)
						close(outChan)
						return
					}
					outChan <- buf
				} else {
					panic("no such channel index")
				}
				// We don't add a way to explicitly stop the
				// go-routine, otherwise the service will
				// block. The service should close the channel
				// when it has nothing else to say because it
				// is the producer. Then this go-routine will
				// be stopped as well.
			}
		}()
		return nil, &StreamingTunnel{outChan, stopServiceChan}, nil
	}

	buf, err = protobuf.Encode(reply)
	if err != nil {
		log.Error(err)
		return nil, nil, errors.New("")
	}
	return buf, nil, nil
}
