package httpsched

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/mesos/mesos-go"
	"github.com/mesos/mesos-go/backoff"
	//"github.com/mesos/mesos-go/encoding"
	"github.com/mesos/mesos-go/httpcli"
	"github.com/mesos/mesos-go/scheduler"
)

const (
	headerMesosStreamID        = "Mesos-Stream-Id"
	debug                      = false
	defaultMaxRedirectAttempts = 9 // per-Do invocation
)

var (
	errMissingMesosStreamId = errors.New("missing Mesos-Stream-Id header expected with successful SUBSCRIBE")
	errNotHTTP              = errors.New("expected an HTTP object, found something else instead")

	// MinRedirectBackoffPeriod MUST be set to some non-zero number, otherwise redirects will panic
	MinRedirectBackoffPeriod = 100 * time.Millisecond
	// MaxRedirectBackoffPeriod SHOULD be set to a value greater than MinRedirectBackoffPeriod
	MaxRedirectBackoffPeriod = 13 * time.Second
)

type (
	withTemporary interface {
		// WithTemporary configures the Client with the temporary option and returns the results of
		// invoking f(). Changes made to the Client by the temporary option are reverted before this
		// func returns.
		WithTemporary(opt httpcli.Opt, f func() error) error
	}

	client struct {
		*httpcli.Client
		maxRedirects int
	}

	Caller interface {
		// CallNoData is for scheduler calls that are not expected to return any data from the server.
		CallNoData(*scheduler.Call) error
		// Call issues a call to Mesos and properly manages call-specific HTTP response headers & data.
		// Subscriptions that succeed return a non-nil Caller that should be used for duration of the subscription.
		Call(*scheduler.Call) (mesos.Response, Caller, error)
	}

	// Client is the public interface this framework scheduler's should consume
	Client interface {
		withTemporary
		Caller
	}

	Option func(*client) Option

	callerTemporary struct {
		temp     httpcli.Opt
		delegate Client
	}
)

var _ = Caller(&callerTemporary{}) // callerTemporary implements Caller

func (ct *callerTemporary) CallNoData(call *scheduler.Call) (err error) {
	return ct.delegate.WithTemporary(ct.temp, func() error {
		return ct.delegate.CallNoData(call)
	})
}

func (ct *callerTemporary) Call(call *scheduler.Call) (resp mesos.Response, caller Caller, err error) {
	ct.delegate.WithTemporary(ct.temp, func() error {
		resp, caller, err = ct.delegate.Call(call)
		return nil
	})
	return
}

// MaxRedirects is a functional option that sets the maximum number of per-call HTTP redirects for a scheduler client
func MaxRedirects(mr int) Option {
	return func(c *client) Option {
		old := c.maxRedirects
		c.maxRedirects = mr
		return MaxRedirects(old)
	}
}

// NewClient returns a scheduler API Client
func NewClient(cl *httpcli.Client, opts ...Option) Client {
	result := &client{Client: cl, maxRedirects: defaultMaxRedirectAttempts}
	cl.With(result.redirectHandler())
	for _, o := range opts {
		if o != nil {
			o(result)
		}
	}
	return result
}

// CallNoData implements Client
func (cli *client) CallNoData(call *scheduler.Call) error {
	resp, err := cli.callWithRedirect(func() (mesos.Response, error) {
		return cli.Do(call)
	})
	if resp != nil {
		resp.Close()
	}
	return err
}

// Call implements Client
func (cli *client) Call(call *scheduler.Call) (resp mesos.Response, caller Caller, err error) {
	opt, requestOpt, optGen := cli.prepare(call)
	cli.WithTemporary(opt, func() error {
		resp, err = cli.callWithRedirect(func() (mesos.Response, error) {
			return cli.Do(call, requestOpt...)
		})
		return nil
	})
	if maybeOpt := optGen(); maybeOpt != nil {
		caller = &callerTemporary{temp: maybeOpt, delegate: cli}
	}
	return
}

var defaultOptGen = func() httpcli.Opt { return nil }

// prepare is invoked for scheduler call's that require pre-processing, post-processing, or both.
func (cli *client) prepare(call *scheduler.Call) (undoable httpcli.Opt, requestOpt []httpcli.RequestOpt, optGen func() httpcli.Opt) {
	optGen = defaultOptGen
	switch call.GetType() {
	case scheduler.Call_SUBSCRIBE:
		mesosStreamID := ""
		undoable = httpcli.WrapDoer(func(f httpcli.DoFunc) httpcli.DoFunc {
			return func(req *http.Request) (*http.Response, error) {
				if debug {
					log.Println("wrapping request")
				}
				resp, err := f(req)
				if debug && err == nil {
					log.Printf("status %d", resp.StatusCode)
					for k := range resp.Header {
						log.Println("header " + k + ": " + resp.Header.Get(k))
					}
				}
				if err == nil && resp.StatusCode == 200 {
					// grab Mesos-Stream-Id header; if missing then
					// close the response body and return an error
					mesosStreamID = resp.Header.Get(headerMesosStreamID)
					if mesosStreamID == "" {
						resp.Body.Close()
						return nil, errMissingMesosStreamId
					}
					if debug {
						log.Println("found mesos-stream-id: " + mesosStreamID)
					}
				}
				return resp, err
			}
		})
		requestOpt = []httpcli.RequestOpt{httpcli.Close(true)}
		optGen = func() httpcli.Opt { return httpcli.DefaultHeader(headerMesosStreamID, mesosStreamID) }
	default:
		// there are no other, special calls that generate data and require pre/post processing
	}
	return
}

type mesosRedirectionError struct {
	newURL string
}

func (mre *mesosRedirectionError) Error() string {
	return "mesos server sent redirect to: " + mre.newURL
}

// redirectHandler returns a config options that transforms normal Mesos redirect "errors" into
// mesosRedirectionErrors by parsing the Location header and computing the address of the next
// endpoint that should be used to replay the failed HTTP request.
func (cli *client) redirectHandler() httpcli.Opt {
	return httpcli.HandleResponse(func(hres *http.Response, err error) (mesos.Response, error) {
		resp, err := cli.HandleResponse(hres, err) // default response handler
		if err == nil || (err != nil && err != httpcli.ErrNotLeader) {
			return resp, err
		}
		res, ok := resp.(*httpcli.Response)
		if !ok {
			if resp != nil {
				resp.Close()
			}
			return nil, errNotHTTP
		}
		// TODO(jdef) refactor this
		// mesos v0.29 will actually send back fully-formed URLs in the Location header
		log.Println("master changed?")
		location, ok := buildNewEndpoint(res.Header.Get("Location"), cli.Endpoint())
		if !ok {
			// TODO(jdef) should probably include the value of the Location header in the message here
			return nil, errors.New("failed to build new Mesos service endpoint URL from Location header")
		}
		res.Close()
		return nil, &mesosRedirectionError{location}
	})
}

// callWithRedirect executes a scheduler Call, following possible HTTP redirects upon changes
// to Mesos leadership. NOTE: this implementation will change the state of the client upon Mesos
// leadership changes.
// @deprecated
func (cli *client) callWithRedirect(f func() (mesos.Response, error)) (resp mesos.Response, err error) {
	var (
		done            chan struct{} // avoid allocating these chans unless we actually need to redirect
		redirectBackoff <-chan struct{}
		getBackoff      = func() <-chan struct{} {
			if redirectBackoff == nil {
				done = make(chan struct{})
				redirectBackoff = backoff.Notifier(MinRedirectBackoffPeriod, MaxRedirectBackoffPeriod, done)
			}
			return redirectBackoff
		}
	)
	defer func() {
		if done != nil {
			close(done)
		}
	}()
	for attempt := 0; ; attempt++ {
		resp, err = f()
		redirectErr, ok := err.(*mesosRedirectionError)
		if !ok {
			return resp, err
		}
		// TODO(jdef) refactor this
		// mesos v0.29 will actually send back fully-formed URLs in the Location header
		if attempt < cli.maxRedirects {
			log.Println("redirecting to " + redirectErr.newURL)
			cli.With(httpcli.Endpoint(redirectErr.newURL))
			<-getBackoff()
			continue
		}
		return
	}
}

func buildNewEndpoint(location, currentEndpoint string) (string, bool) {
	if location == "" {
		return "", false
	}
	// current format appears to be //x.y.z.w:port
	hostport, parseErr := url.Parse(location)
	if parseErr != nil || hostport.Host == "" {
		return "", false
	}
	current, parseErr := url.Parse(currentEndpoint)
	if parseErr != nil {
		return "", false
	}
	current.Host = hostport.Host
	return current.String(), true
}
