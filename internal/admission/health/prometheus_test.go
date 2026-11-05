package health

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func contract() Contract {
	return Contract{Domain: "worker", Revision: "v1", Profile: "followup", WindowSeconds: 2, DeadlineSeconds: 1}
}

func TestHTTPSQueryRequiresTrustedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(payload()) }))
	defer server.Close()
	untrusted, err := NewPrometheus(server.URL, "", contract(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	if _, err = untrusted.Read(context.Background()); err == nil {
		t.Fatal("untrusted query certificate accepted")
	}
	path := filepath.Join(t.TempDir(), "query-ca.pem")
	if err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	trusted, err := NewPrometheus(server.URL, path, contract(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	if _, err = trusted.Read(context.Background()); err != nil {
		t.Fatal("trusted certificate rejected", err)
	}
}
func payload() map[string]any {
	s := Sample{Contract: contract(), End: time.Unix(2000, 0), SourceStarted: time.Unix(1000, 0), Sequence: 1, Planned: 2, Timely: 2, Valid: true}
	rows := []any{}
	for field, v := range Fields(s) {
		rows = append(rows, map[string]any{"metric": map[string]string{"__name__": Metric, "domain": "worker", "revision": "v1", "profile": "followup", "field": field}, "value": []any{2001, strings.TrimSpace(jsonNumber(v))}})
	}
	return map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}}
}
func jsonNumber(v float64) string { data, _ := json.Marshal(v); return string(data) }
func TestDecodeCompleteSnapshotAndFailClosedCases(t *testing.T) {
	data, _ := json.Marshal(payload())
	s, err := Decode(data, contract())
	if err != nil || s.Planned != 2 || s.Reason(contract(), time.Unix(2001, 0), 5*time.Second, 2) != "ok" {
		t.Fatal(s, err)
	}
	for _, name := range []string{"missing", "duplicate", "nan", "reset", "contract", "warnings", "fraction", "timestamp"} {
		t.Run(name, func(t *testing.T) {
			p := payload()
			d := p["data"].(map[string]any)
			rows := d["result"].([]any)
			row := rows[0].(map[string]any)
			switch name {
			case "missing":
				d["result"] = rows[:10]
			case "duplicate":
				rows[10] = rows[0]
			case "nan":
				row["value"] = []any{2001, "NaN"}
			case "reset":
				row["value"] = []any{2001, "-1"}
			case "contract":
				row["metric"].(map[string]string)["revision"] = "v2"
			case "warnings":
				p["warnings"] = []string{"partial response"}
			case "fraction":
				row["metric"].(map[string]string)["field"] = "planned"
				row["value"] = []any{2001, "0.5"}
			case "timestamp":
				row["value"] = []any{1999, "1"}
			}
			data, _ := json.Marshal(p)
			if _, err := Decode(data, contract()); err == nil {
				t.Fatal("accepted", name)
			}
		})
	}
}
func TestPrometheusURLQueryAndRedirectIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || !strings.Contains(r.URL.Query().Get("query"), `revision="v1"`) {
			t.Error(r.URL)
		}
		_ = json.NewEncoder(w).Encode(payload())
	}))
	defer server.Close()
	p, err := NewPrometheus(server.URL, "", contract(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err = p.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://example.com", "http://localhost:19090", "https://user:secret@example.com", "https://example.com?query=up"} {
		if _, err := NewPrometheus(endpoint, "", contract(), time.Second); err == nil {
			t.Fatal(endpoint)
		}
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, http.StatusFound) }))
	defer redirect.Close()
	p2, _ := NewPrometheus(redirect.URL, "", contract(), time.Second)
	defer p2.Close()
	if _, err := p2.Read(context.Background()); err == nil {
		t.Fatal("followed redirect")
	}
}
