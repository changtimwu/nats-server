package test

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"
)

func TestOCSPResponder(t *testing.T) {
	const (
		caCert = "../test/configs/certs/ocsp/ca-cert.pem"
		caKey = "../test/configs/certs/ocsp/ca-key.pem"
		userCert = "../test/configs/certs/ocsp/server.crt"
	)

	s := newOCSPResponder(t, caCert, caKey)
	defer s.Close()

	// setOCSPDatabase(t, s.URL, userCert, ocsp.Good)		// FAIL
	setOCSPDatabase(t, s.Addr, userCert, ocsp.Revoked) // PASS

	resp := postOCSPRequest(t, s.Addr, userCert, caCert)
	if got, want := resp.Status, ocsp.Revoked; got != want {
		t.Fatalf("unexpected cert status, got %d, want %d", got, want)
	}
}

func postOCSPRequest(t *testing.T, ocspURL, certPEM, issuerPEM string) *ocsp.Response {
	t.Helper()

	cert := parseCertPEM(t, certPEM)
	issuer := parseCertPEM(t, issuerPEM)

	reqData, err := ocsp.CreateRequest(cert, issuer, nil)
	if err != nil {
		t.Fatalf("failed to create OCSP request: %s", err)
	}

	r := bytes.NewReader(reqData)
	hc := &http.Client{Timeout: 3 * time.Second}

	httpResp, err := hc.Post(ocspURL, "application/ocsp-request", r)
	if err != nil {
		t.Fatalf("failed POST request: %s", err)
	}
	defer httpResp.Body.Close()

	respData, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("failed to read OCSP HTTP response body: %s", err)
	}

	if got, want := httpResp.Status, "200 OK"; got != want {
		t.Error(strings.TrimSpace(string(respData)))
		t.Fatalf("unexpected OCSP HTTP status, got %q, want %q", got, want)
	}

	resp, err := ocsp.ParseResponse(respData, issuer)
	if err != nil {
		t.Fatalf("failed to parse OCSP response: %s", err)
	}

	return resp
}

func setOCSPDatabase(t *testing.T, ocspURL, certPEM string, status int) {
	t.Helper()

	cert := parseCertPEM(t, certPEM)

	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Post(
		fmt.Sprintf("%s/statuses/%s", ocspURL, cert.SerialNumber),
		"",
		strings.NewReader(fmt.Sprint(status)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read OCSP HTTP response body: %s", err)
	}

	if got, want := resp.Status, "200 OK"; got != want {
		t.Error(strings.TrimSpace(string(data)))
		t.Fatalf("unexpected OCSP HTTP set status, got %q, want %q", got, want)
	}
}

func newOCSPResponder(t *testing.T, issuerCertPEM, issuerKeyPEM string) *http.Server {
	t.Helper()
	var mu sync.Mutex
	status := make(map[string]int)

	issuerCert := parseCertPEM(t, issuerCertPEM)
	issuerKey := parseKeyPEM(t, issuerKeyPEM)

	mux := http.NewServeMux()
	// The "/statuses/" endpoint is for directly setting a key-value pair in
	// the CA's status database.
	mux.HandleFunc("/statuses/", func(rw http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		key := r.URL.Path[len("/statuses/"):]
		switch r.Method {
		case "GET":
			mu.Lock()
			n, ok := status[key]
			if !ok {
				n = ocsp.Unknown
			}
			mu.Unlock()

			fmt.Fprintf(rw, "%s %d", key, n)
		case "POST":
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}

			n, err := strconv.Atoi(string(data))
			if err != nil {
				http.Error(rw, err.Error(), http.StatusBadRequest)
				return
			}

			mu.Lock()
			status[key] = n
			mu.Unlock()

			fmt.Fprintf(rw, "%s %d", key, n)
		default:
			http.Error(rw, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
	})
	// The "/" endpoint is for normal OCSP requests. This actually parses an
	// OCSP status request and signs a response with a CA. Lightly based off:
	// https://www.ietf.org/rfc/rfc2560.txt
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		if r.Method != "POST" {
			http.Error(rw, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Content-Type") != "application/ocsp-request" {
			http.Error(rw, "Unsupported Media Type", http.StatusUnsupportedMediaType)
			return
		}

		reqData, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		ocspReq, err := ocsp.ParseRequest(reqData)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}

		mu.Lock()
		n, ok := status[ocspReq.SerialNumber.String()]
		if !ok {
			n = ocsp.Unknown
		}
		mu.Unlock()

		tmpl := ocsp.Response{
			Status:       n,
			SerialNumber: ocspReq.SerialNumber,
		}
		respData, err := ocsp.CreateResponse(issuerCert, issuerCert, tmpl, issuerKey)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}

		rw.Header().Set("Content-Type", "application/ocsp-response")
		rw.Header().Set("Content-Length", fmt.Sprint(len(respData)))

		fmt.Fprint(rw, string(respData))
	})

	srv := &http.Server{
		Addr:    "127.0.0.1:8888",
		Handler: mux,
	}
	go srv.ListenAndServe()
	return srv
}

func parseCertPEM(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block := parsePEM(t, certPEM)

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse cert %s: %s", certPEM, err)
	}
	return cert
}

func parseKeyPEM(t *testing.T, keyPEM string) *rsa.PrivateKey {
	t.Helper()
	block := parsePEM(t, keyPEM)

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse ikey %s: %s", keyPEM, err)
	}
	return key
}

func parsePEM(t *testing.T, pemPath string) *pem.Block {
	t.Helper()
	data, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatal(err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("failed to decode PEM %s", pemPath)
	}
	return block
}