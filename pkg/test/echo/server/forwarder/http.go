// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forwarder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/lucas-clemente/quic-go/http3"
	"golang.org/x/net/http2"

	"istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/echo/common"
)

var _ protocol = &httpProtocol{}

type httpProtocol struct {
	client *http.Client
	do     common.HTTPDoFunc
}

func splitPath(raw string) (url, path string) {
	schemeSep := "://"
	schemeBegin := strings.Index(raw, schemeSep)
	if schemeBegin == -1 {
		return raw, ""
	}
	schemeEnd := schemeBegin + len(schemeSep)
	pathBegin := strings.IndexByte(raw[schemeEnd:], '/')
	if pathBegin == -1 {
		return raw, ""
	}
	return raw[:schemeEnd+pathBegin], raw[schemeEnd+pathBegin:]
}

func (c *httpProtocol) setHost(r *http.Request, host string) {
	r.Host = host

	if r.URL.Scheme == "https" {
		// Set SNI value to be same as the request Host
		// For use with SNI routing tests
		httpTransport, ok := c.client.Transport.(*http.Transport)
		if ok && httpTransport.TLSClientConfig.ServerName == "" {
			httpTransport.TLSClientConfig.ServerName = host
			return
		}

		http2Transport, ok := c.client.Transport.(*http2.Transport)
		if ok && http2Transport.TLSClientConfig.ServerName == "" {
			http2Transport.TLSClientConfig.ServerName = host
			return
		}

		http3Transport, ok := c.client.Transport.(*http3.RoundTripper)
		if ok && http3Transport.TLSClientConfig.ServerName == "" {
			http3Transport.TLSClientConfig.ServerName = host
			return
		}
	}
}

func (c *httpProtocol) makeRequest(ctx context.Context, req *request) (string, error) {
	method := req.Method
	if method == "" {
		method = "GET"
	}

	// Manually split the path from the URL, the http.NewRequest() will fail to parse paths with invalid encoding that we
	// intentionally used in the test.
	u, p := splitPath(req.URL)
	httpReq, err := http.NewRequest(method, u, nil)
	if err != nil {
		return "", err
	}
	// Use raw path, we don't want golang normalizing anything since we use this for testing purposes
	httpReq.URL.Opaque = p

	// Set the per-request timeout.
	ctx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	httpReq = httpReq.WithContext(ctx)

	var outBuffer bytes.Buffer
	outBuffer.WriteString(fmt.Sprintf("[%d] Url=%s\n", req.RequestID, req.URL))
	host := ""
	writeHeaders(req.RequestID, req.Header, outBuffer, func(key string, value string) {
		if key == hostHeader {
			host = value
		} else {
			// Avoid using .Add() to allow users to pass non-canonical forms
			httpReq.Header[key] = append(httpReq.Header[key], value)
		}
	})

	c.setHost(httpReq, host)

	httpResp, err := c.do(c.client, httpReq)
	if err != nil {
		return outBuffer.String(), err
	}

	outBuffer.WriteString(fmt.Sprintf("[%d] %s=%d\n", req.RequestID, echo.StatusCodeField, httpResp.StatusCode))

	keys := []string{}
	for k := range httpResp.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := httpResp.Header[key]
		for _, value := range values {
			outBuffer.WriteString(fmt.Sprintf("[%d] %s=%s:%s\n", req.RequestID, echo.ResponseHeaderField, key, value))
		}
	}

	data, err := io.ReadAll(httpResp.Body)
	defer func() {
		if err = httpResp.Body.Close(); err != nil {
			outBuffer.WriteString(fmt.Sprintf("[%d error] %s\n", req.RequestID, err))
		}
	}()

	if err != nil {
		return outBuffer.String(), err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			outBuffer.WriteString(fmt.Sprintf("[%d body] %s\n", req.RequestID, line))
		}
	}

	return outBuffer.String(), nil
}

func (c *httpProtocol) Close() error {
	c.client.CloseIdleConnections()
	return nil
}
