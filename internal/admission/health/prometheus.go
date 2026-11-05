package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Source interface {
	Read(context.Context) (Sample, error)
}

type Prometheus struct {
	contract  Contract
	endpoint  string
	client    *http.Client
	transport *http.Transport
}

func NewPrometheus(endpoint, caFile string, c Contract, timeout time.Duration) (*Prometheus, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || !c.Valid() || timeout < time.Millisecond || timeout > 10*time.Second {
		return nil, errors.New("invalid health query configuration")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("health queries require HTTPS or explicit loopback HTTP")
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		file, e := os.Open(caFile)
		if e != nil {
			return nil, errors.New("invalid health CA")
		}
		data, e := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		_ = file.Close()
		if e != nil || len(data) > 1<<20 {
			return nil, errors.New("invalid health CA")
		}
		roots, _ := x509.SystemCertPool()
		if roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("invalid health CA")
		}
		tlsConfig.RootCAs = roots
	}
	t := &http.Transport{TLSClientConfig: tlsConfig, DialContext: (&net.Dialer{Timeout: timeout}).DialContext, DisableCompression: true, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, MaxResponseHeaderBytes: 8192}
	return &Prometheus{contract: c, endpoint: strings.TrimRight(endpoint, "/"), transport: t, client: &http.Client{Transport: t, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *Prometheus) Close() { p.transport.CloseIdleConnections() }
func (p *Prometheus) Read(ctx context.Context) (Sample, error) {
	query := Metric + `{domain="` + p.contract.Domain + `",revision="` + p.contract.Revision + `",profile="` + p.contract.Profile + `"}`
	u := p.endpoint + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	r, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return Sample{}, errors.New("query_request")
	}
	response, err := p.client.Do(r)
	if err != nil {
		return Sample{}, errors.New("query_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return Sample{}, errors.New("query_rejected")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil || len(data) > 256<<10 {
		return Sample{}, errors.New("query_invalid")
	}
	return Decode(data, p.contract)
}

// Decode requires exactly one complete source/window, including identical
// evaluation timestamps. Duplicate exporters, partial scrapes and NaN fail closed.
func Decode(data []byte, c Contract) (Sample, error) {
	var response struct {
		Status   string   `json:"status"`
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
		Data     struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	bad := errors.New("query_invalid")
	if json.Unmarshal(data, &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) > 0 || len(response.Infos) > 0 || len(response.Data.Result) != 11 {
		return Sample{}, bad
	}
	values := map[string]float64{}
	stamp := ""
	identity := ""
	for _, entry := range response.Data.Result {
		if entry.Metric["__name__"] != Metric || entry.Metric["domain"] != c.Domain || entry.Metric["revision"] != c.Revision || entry.Metric["profile"] != c.Profile || len(entry.Value) != 2 {
			return Sample{}, bad
		}
		field := entry.Metric["field"]
		labels := map[string]string{}
		for key, value := range entry.Metric {
			if key != "field" {
				labels[key] = value
			}
		}
		encoded, _ := json.Marshal(labels)
		if identity != "" && identity != string(encoded) {
			return Sample{}, bad
		}
		identity = string(encoded)
		if _, exists := values[field]; exists {
			return Sample{}, bad
		}
		if stamp != "" && stamp != string(entry.Value[0]) {
			return Sample{}, bad
		}
		stamp = string(entry.Value[0])
		var evaluation float64
		if json.Unmarshal(entry.Value[0], &evaluation) != nil || evaluation <= 0 || math.IsNaN(evaluation) || math.IsInf(evaluation, 0) {
			return Sample{}, bad
		}
		var text string
		if json.Unmarshal(entry.Value[1], &text) != nil {
			return Sample{}, bad
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return Sample{}, bad
		}
		values[field] = v
	}
	for _, field := range []string{"window_end_seconds", "source_started_seconds", "sequence", "window_seconds", "deadline_seconds", "planned", "timely_correct", "errors", "timeouts", "rejected", "valid"} {
		v, exists := values[field]
		if !exists {
			return Sample{}, bad
		}
		if field != "window_end_seconds" && field != "source_started_seconds" && !finiteInteger(v) {
			return Sample{}, bad
		}
		if v >= 1<<53 {
			return Sample{}, bad
		}
	}
	if values["window_seconds"] != float64(c.WindowSeconds) || values["deadline_seconds"] != float64(c.DeadlineSeconds) || values["valid"] > 1 {
		return Sample{}, bad
	}
	return Sample{Contract: c, End: time.Unix(0, int64(values["window_end_seconds"]*1e9)), SourceStarted: time.Unix(0, int64(values["source_started_seconds"]*1e9)), Sequence: int64(values["sequence"]), Planned: int64(values["planned"]), Timely: int64(values["timely_correct"]), Errors: int64(values["errors"]), Timeouts: int64(values["timeouts"]), Rejected: int64(values["rejected"]), Valid: values["valid"] == 1}, nil
}
