package cloudflare

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// do() decides success for both axes, so what it makes of a response shape is
// worth pinning directly rather than only through the callers.

// Not every v4 route sends the {success, errors} envelope: `DELETE
// /zones/{zone}/dns_records/{id}` answers with a bare result. Read as a failed
// envelope, that reports a deletion that happened as one that didn't — which is
// the whole point of Teardown being able to say "already gone" honestly.
func TestDo_EnvelopelessSuccessIsNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"id":"023e105f4ecef8ad9ca31a8372d0c353"}}`))
	}))
	defer srv.Close()

	c := newClient("cf_token", nil)
	c.baseURL = srv.URL
	raw, err := c.do(context.Background(), http.MethodDelete, "/zones/z/dns_records/r", nil)
	if err != nil {
		t.Fatalf("a 2xx with no envelope is a success: %v", err)
	}
	if !strings.Contains(string(raw), "023e105f4ecef8ad9ca31a8372d0c353") {
		t.Errorf("the body should come back for decoding, got %q", raw)
	}
}

// The same question one step further out: no body at all. json.Unmarshal fails
// on "", so this has to be answered before the decode or a 204 reports as a
// failure — the deletion-that-happened-read-as-one-that-didn't again, arriving
// through the door the *bool cannot cover.
//
// Note what would have caught it the first time: this is
// TestDo_EnvelopelessSuccessIsNotAFailure with the fake writing nothing. A fake
// that models the shape you assumed makes the suite assert your model.
func TestDo_EmptyBodyOn2xxIsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		c := newClient("cf_token", nil)
		c.baseURL = srv.URL
		if _, err := c.do(context.Background(), http.MethodDelete, "/zones/z/dns_records/r", nil); err != nil {
			t.Errorf("status %d with an empty body is a success, got %v", status, err)
		}
		srv.Close()
	}
}

// …and an empty body on a non-2xx is still a failure, and specifically not one
// that reads as an absent record — Teardown would call that a deletion.
func TestDo_EmptyBodyOnFailureIsNotAnAbsentRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newClient("cf_token", nil)
	c.baseURL = srv.URL
	_, err := c.do(context.Background(), http.MethodDelete, "/zones/z/dns_records/r", nil)
	if err == nil {
		t.Fatal("a 404 with an empty body is not a success")
	}
	if dnsRecordNotFound(err) {
		t.Error("a bare 404 carries no record code, so it is not proof the record is gone")
	}
}

// …but only on a 2xx. Without an envelope to judge by, the transport status is
// the only answer there is, and it must not be ignored.
func TestDo_EnvelopelessFailureIsStillAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"result":null}`))
	}))
	defer srv.Close()

	c := newClient("cf_token", nil)
	c.baseURL = srv.URL
	if _, err := c.do(context.Background(), http.MethodDelete, "/zones/z/dns_records/r", nil); err == nil {
		t.Fatal("a 502 with no envelope is not a success")
	}
}

// The envelope still decides wherever one is sent — including a 200 that carries
// success:false, which Cloudflare does return.
func TestDo_EnvelopeStillDecides(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		code    int
	}{
		{"success true", http.StatusOK, `{"success":true,"result":{}}`, false, 0},
		{"success false on a 200", http.StatusOK, `{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`, true, 81044},
		{"success false with no errors", http.StatusOK, `{"success":false,"errors":[]}`, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := newClient("cf_token", nil)
			c.baseURL = srv.URL
			_, err := c.do(context.Background(), http.MethodGet, "/zones/z/dns_records", nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.code == 0 {
				return
			}
			var ae *apiError
			if !errors.As(err, &ae) || ae.Code != tc.code {
				t.Errorf("want the envelope's code %d carried on the error, got %v", tc.code, err)
			}
		})
	}
}

// A body that isn't the envelope at all — an HTML error page from something in
// front of the API — keeps its text, so the message says what came back.
func TestDo_NonJSONBodyKeepsItsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := newClient("cf_token", nil)
	c.baseURL = srv.URL
	_, err := c.do(context.Background(), http.MethodGet, "/zones/z/dns_records", nil)
	if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("want the body surfaced, got %v", err)
	}
	// And it must not be mistaken for an absent record: Teardown reads that as
	// a successful deletion.
	if dnsRecordNotFound(err) {
		t.Error("an unparseable error body is not proof the record is gone")
	}
}
