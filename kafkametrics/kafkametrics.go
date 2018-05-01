// Package kafkametrics fetches Kafka
// broker metrics and posts events to
// supported metrics backends.
package kafkametrics

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	dd "github.com/zorkian/go-datadog-api"
)

// Config holds Handler
// configuration parameters.
type Config struct {
	// Datadog API key.
	APIKey string
	// Datadog app key.
	AppKey string
	// NetworkTXQuery is a query string
	// that should return the outbound
	// network metrics for the reference
	// Kafka brokers.
	// For example (Datadog): avg:system.net.bytes_sent{service:kafka}".
	NetworkTXQuery string
	// BrokerIDTag is the tag name that
	// Kafka broker ID host tags. For exammple,
	// "host" would ultimately become "by {host}".
	BrokerIDTag string
	// MetricsWindow specifies the window size of
	// timeseries data to evaluate in seconds.
	// All values for the window are averaged.
	MetricsWindow int
}

// Handler requests broker metrics
// and posts events.
type Handler interface {
	GetMetrics() (BrokerMetrics, error)
	PostEvent(*Event) error
}

type ddHandler struct {
	c             *dd.Client
	netTXQuery    string
	brokerIDTag   string
	metricsWindow int
}

// BrokerMetrics is a map of broker IDs
// to *Broker structs.
type BrokerMetrics map[int]*Broker

// Broker holds metrics and metadata
// for a Kafka broker.
type Broker struct {
	ID           int
	Host         string
	InstanceType string
	NetTX        float64
}

// APIError wraps backend
// metric system errors.
type APIError struct {
	request string
	err     string
}

// Error implements the error
// interface for APIError.
func (e *APIError) Error() string {
	return fmt.Sprintf("API error [%s]: %s", e.request, e.err)
}

// PartialResults types are returned
// when incomplete broker metrics or
// metadata is returned.
type PartialResults struct {
	err string
}

// Error implements the error
// interface for PartialResults.
func (e *PartialResults) Error() string {
	return e.err
}

// Event is used to post autothrottle
// events to the backend metrics system.
type Event struct {
	Title string
	Text  string
	Tags  []string
}

// PostEvent posts an event to the
// Datadog API.
func (k *ddHandler) PostEvent(e *Event) error {
	m := &dd.Event{
		Title: &e.Title,
		Text:  &e.Text,
		Tags:  e.Tags,
	}

	_, err := k.c.PostEvent(m)
	return err
}

// NewHandler takes a *Config and
// returns a Handler, along with
// any credential validation errors.
// Further backends can be supported with
// a type switch and some other changes.
func NewHandler(c *Config) (Handler, error) {
	client := dd.NewClient(c.APIKey, c.AppKey)

	// Validate.
	ok, err := client.Validate()
	if err != nil {
		return nil, &APIError{
			request: "validate credentials",
			err:     err.Error(),
		}
	}

	if !ok {
		return nil, &APIError{
			request: "validate credentials",
			err:     "invalid API or app key",
		}
	}

	netQ := createNetTXQuery(c)

	k := &ddHandler{
		c:             client,
		netTXQuery:    netQ,
		metricsWindow: c.MetricsWindow,
		brokerIDTag: c.BrokerIDTag,
	}

	return k, nil
}

// createNetTXQuery takes a metric query
// with no aggs plus a window in seconds. A full
// metric query is returned with an avg rollup
// for the provided window.
func createNetTXQuery(c *Config) string {
	var b bytes.Buffer
	b.WriteString(c.NetworkTXQuery)
	b.WriteString(fmt.Sprintf(".rollup(avg, %d)", c.MetricsWindow))
	return b.String()
}

// GetMetrics requests broker metrics and metadata
// from the Datadog API and returns a BrokerMetrics.
func (k *ddHandler) GetMetrics() (BrokerMetrics, error) {
	// Get series.
	start := time.Now().Add(-time.Duration(k.metricsWindow) * time.Second).Unix()
	o, err := k.c.QueryMetrics(start, time.Now().Unix(), k.netTXQuery)
	if err != nil {
		return nil, &APIError{
			request: "metrics query",
			err:     err.Error(),
		}
	}

	if len(o) == 0 {
		return nil, &PartialResults{
			err: fmt.Sprintf("No data returned with query %s", k.netTXQuery),
		}
	}

	// Get a []*Broker from the series.
	blist, err := brokersFromSeries(o)
	if err != nil {
		return nil, err
	}
	// The []*Broker only contains hostnames
	// and the network tx metric. Fetch the rest
	// of the required metadata and construct
	// a BrokerMetrics.
	return k.brokerMetricsFromList(blist)
}

// brokersFromSeries takes metrics series as a
// []dd.Series and returns a []*Broker. An error
// is returned if for some reason no points were
// returned with the series.
func brokersFromSeries(s []dd.Series) ([]*Broker, error) {
	bs := []*Broker{}
	for _, ts := range s {
		host := tagValFromScope(ts.GetScope(), "host")

		if len(ts.Points) == 0 {
			return nil, &PartialResults{
				err: fmt.Sprintf("no points for host %s", host),
			}
		}

		b := &Broker{
			Host:  host,
			NetTX: *ts.Points[0][1] / 1024 / 1024,
		}

		bs = append(bs, b)
	}

	return bs, nil
}

// brokerMetricsFromList takes a *[]Broker and fetches
// relevant host tags for all brokers in the list, returning
// a BrokerMetrics.
func (k *ddHandler) brokerMetricsFromList(l []*Broker) (BrokerMetrics, error) {
	// Get host tags for brokers
	// in the list.
	tags, err := k.getHostTagMap(l)
	if err != nil {
		return nil, err
	}

	brokers := BrokerMetrics{}
	err = brokers.populateFromTagMap(tags, k.brokerIDTag)
	if err != nil {
		return nil, err
	}

	return brokers, nil
}

// getHostTagsMulti takes a []*Broker and fetches
// host tags for each. If no errors are encountered,
// a map[*Broker][]string holding the received tags
// is returned.
func (k *ddHandler) getHostTagMap(l []*Broker) (map[*Broker][]string, error) {
	brokers := map[*Broker][]string{}
	// Get broker IDs for each host,
	// populate into a BrokerMetrics.
	for _, b := range l {
		ht, err := k.c.GetHostTags(b.Host, "")
		if err != nil {
			return nil, &APIError{
				request: "host tags",
				err:     fmt.Sprintf("Error requesting host tags for %s", b.Host),
			}
		}

		brokers[b] = ht
	}

	return brokers, nil
}

// populateFromTagMap takes a map of broker tags
// and a broker ID tag key and returns a BrokerMetrics
// with tags of interest. An error describing any
// missing tags is returned.
func (bm BrokerMetrics) populateFromTagMap(t map[*Broker][]string, btag string) error {
	var missingTags bytes.Buffer

	for b, ht := range t {
		ids := valFromTags(ht, btag)
		if ids != "" {
			id, _ := strconv.Atoi(ids)
			b.ID = id
			bm[id] = b
		} else {
			s := fmt.Sprintf(" %s:%s", btag, b.Host)
			missingTags.WriteString(s)
		}

		it := valFromTags(ht, "instance-type")
		if it != "" {
			bm[b.ID].InstanceType = it
		} else {
			s := fmt.Sprintf(" instance_type:%s", b.Host)
			missingTags.WriteString(s)
		}
	}

	if missingTags.String() != "" {
		return &PartialResults{
			err: fmt.Sprintf("Host tags missing for: %s", missingTags.String()),
		}
	}

	return nil
}

// tagValFromScope takes a metric scope string
// and a tag and returns that tag's value.
func tagValFromScope(scope, tag string) string {
	ts := strings.Split(scope, ",")

	return valFromTags(ts, tag)
}

// valFromTags takes a []string of tags and
// a key, returning the value for the key.
func valFromTags(tags []string, key string) string {
	var v []string

	for _, tag := range tags {
		if strings.HasPrefix(tag, key+":") {
			v = strings.Split(tag, ":")
			break
		}
	}

	if len(v) > 1 {
		return v[1]
	}

	return ""
}
