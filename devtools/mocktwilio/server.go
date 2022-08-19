package mocktwilio

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/errors"
	"github.com/target/goalert/notification/twilio"
	"github.com/ttacon/libphonenumber"
)

// Config is used to configure the mock server.
type Config struct {
	// AccountSID is the Twilio account SID.
	AccountSID string

	// AuthToken is the Twilio auth token.
	AuthToken string

	// If EnableAuth is true, incoming requests will need to have a valid Authorization header.
	EnableAuth bool

	OnError func(context.Context, error)
}

// Number represents a mock phone number.
type Number struct {
	Number string

	VoiceWebhookURL string
	SMSWebhookURL   string
}

// MsgService allows configuring a mock messaging service that can rotate between available numbers.
type MsgService struct {
	// ID is the messaging service SID, it must start with 'MG'.
	ID string

	Numbers []string

	// SMSWebhookURL is the URL to which SMS messages will be sent.
	//
	// It takes precedence over the SMSWebhookURL field in the Config.Numbers field
	// for all numbers in the service.
	SMSWebhookURL string
}

// Server implements the Twilio API for SMS and Voice calls
// via the http.Handler interface.
type Server struct {
	cfg Config

	msgCh         chan Message
	msgStateDB    chan map[string]*msgState
	outboundMsgCh chan *msgState

	callsCh        chan Call
	callStateDB    chan map[string]*callState
	outboundCallCh chan *callState

	numbersDB chan map[string]*Number
	msgSvcDB  chan map[string][]*Number

	waitInFlight chan chan struct{}

	mux *http.ServeMux

	once         sync.Once
	shutdown     chan struct{}
	shutdownDone chan struct{}

	id uint64

	workers sync.WaitGroup

	carrierInfo   map[string]twilio.CarrierInfo
	carrierInfoMx sync.Mutex
}

func validateURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme == "" {
		return errors.Errorf("invalid URL (missing scheme): %s", s)
	}

	return nil
}

// NewServer creates a new Server.
func NewServer(cfg Config) *Server {
	if cfg.AccountSID == "" {
		panic("AccountSID is required")
	}

	srv := &Server{
		cfg:       cfg,
		msgSvcDB:  make(chan map[string][]*Number, 1),
		numbersDB: make(chan map[string]*Number, 1),
		mux:       http.NewServeMux(),

		msgCh:         make(chan Message, 10000),
		msgStateDB:    make(chan map[string]*msgState, 1),
		outboundMsgCh: make(chan *msgState),

		callsCh:        make(chan Call, 10000),
		callStateDB:    make(chan map[string]*callState, 1),
		outboundCallCh: make(chan *callState),

		shutdown:     make(chan struct{}),
		shutdownDone: make(chan struct{}),

		waitInFlight: make(chan chan struct{}),
	}
	srv.msgSvcDB <- make(map[string][]*Number)
	srv.numbersDB <- make(map[string]*Number)
	srv.msgStateDB <- make(map[string]*msgState)

	srv.initHTTP()

	go srv.loop()

	return srv
}

func (srv *Server) number(s string) *Number {
	db := <-srv.numbersDB
	n := db[s]
	srv.numbersDB <- db
	return n
}

func (srv *Server) numberSvc(id string) []*Number {
	db := <-srv.msgSvcDB
	nums := db[id]
	srv.msgSvcDB <- db

	return nums
}

// AddNumber adds a new number to the mock server.
func (srv *Server) AddNumber(n Number) error {
	_, err := libphonenumber.Parse(n.Number, "")
	if err != nil {
		return fmt.Errorf("invalid phone number %s: %v", n.Number, err)
	}
	if n.SMSWebhookURL != "" {
		err = validateURL(n.SMSWebhookURL)
		if err != nil {
			return err
		}
	}
	if n.VoiceWebhookURL != "" {
		err = validateURL(n.VoiceWebhookURL)
		if err != nil {
			return err
		}
	}

	db := <-srv.numbersDB
	if _, ok := db[n.Number]; ok {
		srv.numbersDB <- db
		return fmt.Errorf("number %s already exists", n.Number)
	}
	db[n.Number] = &n
	srv.numbersDB <- db
	return nil
}

// AddMsgService adds a new messaging service to the mock server.
func (srv *Server) AddMsgService(ms MsgService) error {
	if !strings.HasPrefix(ms.ID, "MG") {
		return fmt.Errorf("invalid MsgService SID %s", ms.ID)
	}

	if ms.SMSWebhookURL != "" {
		err := validateURL(ms.SMSWebhookURL)
		if err != nil {
			return err
		}
	}
	for _, nStr := range ms.Numbers {
		_, err := libphonenumber.Parse(nStr, "")
		if err != nil {
			return fmt.Errorf("invalid phone number %s: %v", nStr, err)
		}
	}

	msDB := <-srv.msgSvcDB
	if _, ok := msDB[ms.ID]; ok {
		srv.msgSvcDB <- msDB
		return fmt.Errorf("MsgService SID %s already exists", ms.ID)
	}

	numDB := <-srv.numbersDB
	for _, nStr := range ms.Numbers {
		n := numDB[nStr]
		if n == nil {
			n = &Number{Number: nStr}
			numDB[nStr] = n
		}
		msDB[ms.ID] = append(msDB[ms.ID], n)

		if ms.SMSWebhookURL == "" {
			continue
		}

		n.SMSWebhookURL = ms.SMSWebhookURL
	}
	srv.numbersDB <- numDB
	srv.msgSvcDB <- msDB

	return nil
}

func (srv *Server) nextID(prefix string) string {
	return fmt.Sprintf("%s%032d", prefix, atomic.AddUint64(&srv.id, 1))
}

func (srv *Server) logErr(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if srv.cfg.OnError == nil {
		return
	}

	srv.cfg.OnError(ctx, err)
}

// Close shuts down the server.
func (srv *Server) Close() error {
	srv.once.Do(func() {
		close(srv.shutdown)
	})

	<-srv.shutdownDone
	return nil
}

func (srv *Server) loop() {
	var wg sync.WaitGroup

	defer close(srv.shutdownDone)
	defer close(srv.msgCh)
	defer wg.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		select {
		case <-srv.shutdown:

			return
		case sms := <-srv.outboundMsgCh:
			wg.Add(1)
			go func() {
				sms.lifecycle(ctx)
				wg.Done()
			}()
		case ch := <-srv.waitInFlight:
			go func() {
				wg.Wait()
				close(ch)
			}()
		}
	}
}

// WaitInFlight waits for all in-flight requests/messages/calls to complete.
func (srv *Server) WaitInFlight(ctx context.Context) error {
	ch := make(chan struct{})
	select {
	case srv.waitInFlight <- ch:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}
