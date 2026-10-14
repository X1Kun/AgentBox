package api

import (
	"strings"
	"testing"
)

func TestRequestPolicy(t *testing.T) {
	valid := Request{Runtime: "python", Code: "print('hello')"}
	r, err := valid.Validate()
	if err != nil || r.TimeoutMS != DefaultTimeoutMS {
		t.Fatalf("default: %+v %v", r, err)
	}
	tests := []struct {
		name   string
		change func(*Request)
	}{
		{"runtime", func(r *Request) { r.Runtime = "bash" }},
		{"empty", func(r *Request) { r.Code = " \n" }},
		{"oversize", func(r *Request) { r.Code = strings.Repeat("x", MaxCodeBytes+1) }},
		{"negative_timeout", func(r *Request) { r.TimeoutMS = -1 }},
		{"long_timeout", func(r *Request) { r.TimeoutMS = MaxTimeoutMS + 1 }},
		{"host_environment", func(r *Request) { r.Env = map[string]string{"LD_PRELOAD": "/tmp/a.so"} }},
		{"nul", func(r *Request) { r.Env = map[string]string{"AGENT_INPUT": "a\x00b"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid
			tt.change(&r)
			if _, err := r.Validate(); err == nil {
				t.Fatal("unsafe request accepted")
			}
		})
	}
}
