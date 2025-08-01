// Copyright (c) Facebook, Inc. and its affiliates. All Rights Reserved
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

// This file contains code that was borrowed from prestgo, mainly some
// data type definitions.
//
// See https://github.com/avct/prestgo for copyright information.
//
// The MIT License (MIT)
//
// Copyright (c) 2015 Avocet Systems Ltd.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Package trino provides a database/sql driver for Trino.
//
// The driver should be used via the database/sql package:
//
//	import "database/sql"
//	import _ "github.com/trinodb/trino-go-client/trino"
//
//	dsn := "http://user@localhost:8080?catalog=default&schema=test"
//	db, err := sql.Open("trino", dsn)
package trino

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jcmturner/gokrb5/v8/client"
	"github.com/jcmturner/gokrb5/v8/config"
	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/spnego"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4"
)

func init() {
	sql.Register("trino", &Driver{})
}

var (
	// DefaultQueryTimeout is the default timeout for queries executed without a context.
	DefaultQueryTimeout = 10 * time.Hour

	// DefaultCancelQueryTimeout is the timeout for the request to cancel queries in Trino.
	DefaultCancelQueryTimeout = 30 * time.Second

	// ErrOperationNotSupported indicates that a database operation is not supported.
	ErrOperationNotSupported = errors.New("trino: operation not supported")

	// ErrQueryCancelled indicates that a query has been cancelled.
	ErrQueryCancelled = errors.New("trino: query cancelled")

	// ErrUnsupportedHeader indicates that the server response contains an unsupported header.
	ErrUnsupportedHeader = errors.New("trino: server response contains an unsupported header")

	// ErrInvalidResponseType indicates that the server returned an invalid type definition.
	ErrInvalidResponseType = errors.New("trino: server response contains an invalid type")

	// ErrInvalidProgressCallbackHeader indicates that server did not get valid headers for progress callback
	ErrInvalidProgressCallbackHeader = errors.New("trino: both " + trinoProgressCallbackParam + " and " + trinoProgressCallbackPeriodParam + " must be set when using progress callback")
)

const (
	trinoHeaderPrefix = `X-Trino-`

	preparedStatementHeader = trinoHeaderPrefix + "Prepared-Statement"
	preparedStatementName   = "_trino_go"

	trinoUserHeader            = trinoHeaderPrefix + `User`
	trinoSourceHeader          = trinoHeaderPrefix + `Source`
	trinoCatalogHeader         = trinoHeaderPrefix + `Catalog`
	trinoSchemaHeader          = trinoHeaderPrefix + `Schema`
	trinoSessionHeader         = trinoHeaderPrefix + `Session`
	trinoSetCatalogHeader      = trinoHeaderPrefix + `Set-Catalog`
	trinoSetSchemaHeader       = trinoHeaderPrefix + `Set-Schema`
	trinoSetPathHeader         = trinoHeaderPrefix + `Set-Path`
	trinoSetSessionHeader      = trinoHeaderPrefix + `Set-Session`
	trinoClearSessionHeader    = trinoHeaderPrefix + `Clear-Session`
	trinoSetRoleHeader         = trinoHeaderPrefix + `Set-Role`
	trinoExtraCredentialHeader = trinoHeaderPrefix + `Extra-Credential`

	trinoProgressCallbackParam       = trinoHeaderPrefix + `Progress-Callback`
	trinoProgressCallbackPeriodParam = trinoHeaderPrefix + `Progress-Callback-Period`

	trinoAddedPrepareHeader       = trinoHeaderPrefix + `Added-Prepare`
	trinoDeallocatedPrepareHeader = trinoHeaderPrefix + `Deallocated-Prepare`

	trinoQueryDataEncodingHeader = trinoHeaderPrefix + `Query-Data-Encoding`
	trinoEncoding                = "encoding"
	trinoSpoolingWorkerCount     = `spooling_worker_count`
	trinoMaxOutOfOrdersSegments  = `max_out_of_order_segments`

	authorizationHeader = "Authorization"

	kerberosEnabledConfig            = "KerberosEnabled"
	kerberosKeytabPathConfig         = "KerberosKeytabPath"
	kerberosPrincipalConfig          = "KerberosPrincipal"
	kerberosRealmConfig              = "KerberosRealm"
	kerberosConfigPathConfig         = "KerberosConfigPath"
	kerberosRemoteServiceNameConfig  = "KerberosRemoteServiceName"
	sslCertPathConfig                = "SSLCertPath"
	sslCertConfig                    = "SSLCert"
	accessTokenConfig                = "accessToken"
	explicitPrepareConfig            = "explicitPrepare"
	forwardAuthorizationHeaderConfig = "forwardAuthorizationHeader"

	mapKeySeparator   = ":"
	mapEntrySeparator = ";"

	defaultallowedOutOfOrder       = 10
	defaultSpoolingDownloadWorkers = 5
	defaulttrinoEncoding           = "json"
)

var (
	responseToRequestHeaderMap = map[string]string{
		trinoSetSchemaHeader:  trinoSchemaHeader,
		trinoSetCatalogHeader: trinoCatalogHeader,
	}
	unsupportedResponseHeaders = []string{
		trinoSetPathHeader,
		trinoSetRoleHeader,
	}
)

type Driver struct{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	return newConn(name)
}

var _ driver.Driver = &Driver{}

// Config is a configuration that can be encoded to a DSN string.
type Config struct {
	ServerURI                  string            // URI of the Trino server, e.g. http://user@localhost:8080
	Source                     string            // Source of the connection (optional)
	Catalog                    string            // Catalog (optional)
	Schema                     string            // Schema (optional)
	SessionProperties          map[string]string // Session properties (optional)
	ExtraCredentials           map[string]string // Extra credentials (optional)
	CustomClientName           string            // Custom client name (optional)
	KerberosEnabled            string            // KerberosEnabled (optional, default is false)
	KerberosKeytabPath         string            // Kerberos Keytab Path (optional)
	KerberosPrincipal          string            // Kerberos Principal used to authenticate to KDC (optional)
	KerberosRemoteServiceName  string            // Trino coordinator Kerberos service name (optional)
	KerberosRealm              string            // The Kerberos Realm (optional)
	KerberosConfigPath         string            // The krb5 config path (optional)
	SSLCertPath                string            // The SSL cert path for TLS verification (optional)
	SSLCert                    string            // The SSL cert for TLS verification (optional)
	AccessToken                string            // An access token (JWT) for authentication (optional)
	ForwardAuthorizationHeader bool              // Allow forwarding the `accessToken` named query parameter in the authorization header, overwriting the `AccessToken` option, if set (optional)
	QueryTimeout               *time.Duration    // Configurable timeout for query (optional)
}

// FormatDSN returns a DSN string from the configuration.
func (c *Config) FormatDSN() (string, error) {
	serverURL, err := url.Parse(c.ServerURI)
	if err != nil {
		return "", err
	}
	var sessionkv []string
	if c.SessionProperties != nil {
		for k, v := range c.SessionProperties {
			sessionkv = append(sessionkv, k+mapKeySeparator+v)
		}
	}
	var credkv []string
	if c.ExtraCredentials != nil {
		for k, v := range c.ExtraCredentials {
			credkv = append(credkv, k+mapKeySeparator+v)
		}
	}
	source := c.Source
	if source == "" {
		source = "trino-go-client"
	}
	query := make(url.Values)
	query.Add("source", source)

	if c.ForwardAuthorizationHeader {
		query.Add(forwardAuthorizationHeaderConfig, "true")
	}

	KerberosEnabled, _ := strconv.ParseBool(c.KerberosEnabled)
	isSSL := serverURL.Scheme == "https"

	if c.CustomClientName != "" {
		if c.SSLCert != "" || c.SSLCertPath != "" {
			return "", fmt.Errorf("trino: client configuration error, a custom client cannot be specific together with a custom SSL certificate")
		}
	}
	if c.SSLCertPath != "" {
		if !isSSL {
			return "", fmt.Errorf("trino: client configuration error, SSL must be enabled to specify a custom SSL certificate file")
		}
		if c.SSLCert != "" {
			return "", fmt.Errorf("trino: client configuration error, a custom SSL certificate file cannot be specified together with a certificate string")
		}
		query.Add(sslCertPathConfig, c.SSLCertPath)
	}

	if c.SSLCert != "" {
		if !isSSL {
			return "", fmt.Errorf("trino: client configuration error, SSL must be enabled to specify a custom SSL certificate")
		}
		if c.SSLCertPath != "" {
			return "", fmt.Errorf("trino: client configuration error, a custom SSL certificate string cannot be specified together with a certificate file")
		}
		query.Add(sslCertConfig, c.SSLCert)
	}

	if KerberosEnabled {
		if !isSSL {
			return "", fmt.Errorf("trino: client configuration error, SSL must be enabled for secure env")
		}
		query.Add(kerberosEnabledConfig, "true")
		query.Add(kerberosKeytabPathConfig, c.KerberosKeytabPath)
		query.Add(kerberosPrincipalConfig, c.KerberosPrincipal)
		query.Add(kerberosRealmConfig, c.KerberosRealm)
		query.Add(kerberosConfigPathConfig, c.KerberosConfigPath)
		remoteServiceName := c.KerberosRemoteServiceName
		if remoteServiceName == "" {
			remoteServiceName = "trino"
		}
		query.Add(kerberosRemoteServiceNameConfig, remoteServiceName)
	}

	// ensure consistent order of items
	sort.Strings(sessionkv)
	sort.Strings(credkv)

	if c.QueryTimeout != nil {
		query.Add("query_timeout", c.QueryTimeout.String())
	}

	for k, v := range map[string]string{
		"catalog":            c.Catalog,
		"schema":             c.Schema,
		"session_properties": strings.Join(sessionkv, mapEntrySeparator),
		"extra_credentials":  strings.Join(credkv, mapEntrySeparator),
		"custom_client":      c.CustomClientName,
		accessTokenConfig:    c.AccessToken,
	} {
		if v != "" {
			query[k] = []string{v}
		}
	}
	serverURL.RawQuery = query.Encode()
	return serverURL.String(), nil
}

// Conn is a Trino connection.
type Conn struct {
	baseURL                    string
	auth                       *url.Userinfo
	httpClient                 http.Client
	httpHeaders                http.Header
	kerberosEnabled            bool
	kerberosClient             *client.Client
	kerberosRemoteServiceName  string
	progressUpdater            ProgressUpdater
	progressUpdaterPeriod      queryProgressCallbackPeriod
	useExplicitPrepare         bool
	forwardAuthorizationHeader bool
	queryTimeout               *time.Duration
}

var (
	_ driver.Conn               = &Conn{}
	_ driver.ConnPrepareContext = &Conn{}
)

func newConn(dsn string) (*Conn, error) {
	serverURL, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("trino: malformed dsn: %w", err)
	}

	query := serverURL.Query()

	kerberosEnabled, _ := strconv.ParseBool(query.Get(kerberosEnabledConfig))

	forwardAuthorizationHeader, _ := strconv.ParseBool(query.Get(forwardAuthorizationHeaderConfig))

	useExplicitPrepare := true
	if query.Get(explicitPrepareConfig) != "" {
		useExplicitPrepare, _ = strconv.ParseBool(query.Get(explicitPrepareConfig))
	}

	var kerberosClient *client.Client

	if kerberosEnabled {
		kt, err := keytab.Load(query.Get(kerberosKeytabPathConfig))
		if err != nil {
			return nil, fmt.Errorf("trino: Error loading Keytab: %w", err)
		}
		conf, err := config.Load(query.Get(kerberosConfigPathConfig))
		if err != nil {
			return nil, fmt.Errorf("trino: Error loading krb config: %w", err)
		}

		kerberosClient = client.NewWithKeytab(query.Get(kerberosPrincipalConfig), query.Get(kerberosRealmConfig), kt, conf)
		loginErr := kerberosClient.Login()
		if loginErr != nil {
			return nil, fmt.Errorf("trino: Error login to KDC: %v", loginErr)
		}
	}

	var httpClient = http.DefaultClient
	if clientKey := query.Get("custom_client"); clientKey != "" {
		httpClient = getCustomClient(clientKey)
		if httpClient == nil {
			return nil, fmt.Errorf("trino: custom client not registered: %q", clientKey)
		}
	} else if serverURL.Scheme == "https" {

		cert := []byte(query.Get(sslCertConfig))

		if certPath := query.Get(sslCertPathConfig); certPath != "" {
			cert, err = os.ReadFile(certPath)
			if err != nil {
				return nil, fmt.Errorf("trino: Error loading SSL Cert File: %w", err)
			}
		}

		if len(cert) != 0 {
			certPool := x509.NewCertPool()
			certPool.AppendCertsFromPEM(cert)

			httpClient = &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						RootCAs: certPool,
					},
				},
			}
		}
	}

	var queryTimeout *time.Duration
	if timeoutStr := query.Get("query_timeout"); timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return nil, fmt.Errorf("trino: invalid timeout: %w", err)
		}
		queryTimeout = &d
	}

	c := &Conn{
		baseURL:                    serverURL.Scheme + "://" + serverURL.Host,
		httpClient:                 *httpClient,
		httpHeaders:                make(http.Header),
		kerberosClient:             kerberosClient,
		kerberosEnabled:            kerberosEnabled,
		kerberosRemoteServiceName:  query.Get(kerberosRemoteServiceNameConfig),
		useExplicitPrepare:         useExplicitPrepare,
		forwardAuthorizationHeader: forwardAuthorizationHeader,
		queryTimeout:               queryTimeout,
	}

	var user string
	if serverURL.User != nil {
		user = serverURL.User.Username()
		pass, _ := serverURL.User.Password()
		if pass != "" && serverURL.Scheme == "https" {
			c.auth = serverURL.User
		}
	}

	for k, v := range map[string]string{
		trinoUserHeader:     user,
		trinoSourceHeader:   query.Get("source"),
		trinoCatalogHeader:  query.Get("catalog"),
		trinoSchemaHeader:   query.Get("schema"),
		authorizationHeader: getAuthorization(query.Get(accessTokenConfig)),
	} {
		if v != "" {
			c.httpHeaders.Add(k, v)
		}
	}
	for header, param := range map[string]string{
		trinoSessionHeader:         "session_properties",
		trinoExtraCredentialHeader: "extra_credentials",
	} {
		v := query.Get(param)
		if v != "" {
			c.httpHeaders[header], err = decodeMapHeader(param, v)
			if err != nil {
				return c, err
			}
		}
	}

	return c, nil
}

func decodeMapHeader(name, input string) ([]string, error) {
	result := []string{}
	for _, entry := range strings.Split(input, mapEntrySeparator) {
		parts := strings.SplitN(entry, mapKeySeparator, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("trino: Malformed %s: %s", name, input)
		}
		key := parts[0]
		value := parts[1]
		if len(key) == 0 {
			return nil, fmt.Errorf("trino: %s key is empty", name)
		}
		if len(value) == 0 {
			return nil, fmt.Errorf("trino: %s value is empty", name)
		}
		if !isASCII(key) {
			return nil, fmt.Errorf("trino: %s key '%s' contains spaces or is not printable ASCII", name, key)
		}
		if !isASCII(value) {
			// do not log value as it may contain sensitive information
			return nil, fmt.Errorf("trino: %s value for key '%s' contains spaces or is not printable ASCII", name, key)
		}
		result = append(result, key+"="+url.QueryEscape(value))
	}
	return result, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '\u0021' || s[i] > '\u007E' {
			return false
		}
	}
	return true
}

func getAuthorization(token string) string {
	if token == "" {
		return ""
	}
	return fmt.Sprintf("Bearer %s", token)
}

// registry for custom http clients
var customClientRegistry = struct {
	sync.RWMutex
	Index map[string]http.Client
}{
	Index: make(map[string]http.Client),
}

// RegisterCustomClient associates a client to a key in the driver's registry.
//
// Register your custom client in the driver, then refer to it by name in the DSN, on the call to sql.Open:
//
//	foobarClient := &http.Client{
//		Transport: &http.Transport{
//			Proxy: http.ProxyFromEnvironment,
//			DialContext: (&net.Dialer{
//				Timeout:   30 * time.Second,
//				KeepAlive: 30 * time.Second,
//				DualStack: true,
//			}).DialContext,
//			MaxIdleConns:          100,
//			IdleConnTimeout:       90 * time.Second,
//			TLSHandshakeTimeout:   10 * time.Second,
//			ExpectContinueTimeout: 1 * time.Second,
//			TLSClientConfig:       &tls.Config{
//			// your config here...
//			},
//		},
//	}
//	trino.RegisterCustomClient("foobar", foobarClient)
//	db, err := sql.Open("trino", "https://user@localhost:8080?custom_client=foobar")
func RegisterCustomClient(key string, client *http.Client) error {
	if _, err := strconv.ParseBool(key); err == nil {
		return fmt.Errorf("trino: custom client key %q is reserved", key)
	}
	customClientRegistry.Lock()
	customClientRegistry.Index[key] = *client
	customClientRegistry.Unlock()
	return nil
}

// DeregisterCustomClient removes the client associated to the key.
func DeregisterCustomClient(key string) {
	customClientRegistry.Lock()
	delete(customClientRegistry.Index, key)
	customClientRegistry.Unlock()
}

func getCustomClient(key string) *http.Client {
	customClientRegistry.RLock()
	defer customClientRegistry.RUnlock()
	if client, ok := customClientRegistry.Index[key]; ok {
		return &client
	}
	return nil
}

// Begin implements the driver.Conn interface.
func (c *Conn) Begin() (driver.Tx, error) {
	return nil, ErrOperationNotSupported
}

// Prepare implements the driver.Conn interface.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

// PrepareContext implements the driver.ConnPrepareContext interface.
func (c *Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return &driverStmt{conn: c, query: query}, nil
}

// Close implements the driver.Conn interface.
func (c *Conn) Close() error {
	return nil
}

func (c *Conn) newRequest(ctx context.Context, method, url string, body io.Reader, hs http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("trino: %w", err)
	}

	if c.kerberosEnabled {
		remoteServiceName := "trino"
		if c.kerberosRemoteServiceName != "" {
			remoteServiceName = c.kerberosRemoteServiceName
		}
		err = spnego.SetSPNEGOHeader(c.kerberosClient, req, remoteServiceName+"/"+req.URL.Hostname())
		if err != nil {
			return nil, fmt.Errorf("error setting client SPNEGO header: %w", err)
		}
	}

	for k, v := range c.httpHeaders {
		req.Header[k] = v
	}
	for k, v := range hs {
		req.Header[k] = v
	}

	if c.auth != nil {
		pass, _ := c.auth.Password()
		req.SetBasicAuth(c.auth.Username(), pass)
	}
	return req, nil
}

func (c *Conn) roundTrip(ctx context.Context, req *http.Request) (*http.Response, error) {
	delay := 100 * time.Millisecond
	const maxDelayBetweenRequests = float64(15 * time.Second)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			resp, err := c.httpClient.Do(req)
			if err != nil {
				return nil, &ErrQueryFailed{Reason: err}
			}
			switch resp.StatusCode {
			case http.StatusOK:
				for src, dst := range responseToRequestHeaderMap {
					if v := resp.Header.Get(src); v != "" {
						c.httpHeaders.Set(dst, v)
					}
				}
				if v := resp.Header.Get(trinoAddedPrepareHeader); v != "" {
					c.httpHeaders.Add(preparedStatementHeader, v)
				}
				if v := resp.Header.Get(trinoDeallocatedPrepareHeader); v != "" {
					values := c.httpHeaders.Values(preparedStatementHeader)
					c.httpHeaders.Del(preparedStatementHeader)
					for _, v2 := range values {
						if !strings.HasPrefix(v2, v+"=") {
							c.httpHeaders.Add(preparedStatementHeader, v2)
						}
					}
				}
				if v := resp.Header.Get(trinoSetSessionHeader); v != "" {
					c.httpHeaders.Add(trinoSessionHeader, v)
				}
				if v := resp.Header.Get(trinoClearSessionHeader); v != "" {
					values := c.httpHeaders.Values(trinoSessionHeader)
					c.httpHeaders.Del(trinoSessionHeader)
					for _, v2 := range values {
						if !strings.HasPrefix(v2, v+"=") {
							c.httpHeaders.Add(trinoSessionHeader, v2)
						}
					}
				}
				for _, name := range unsupportedResponseHeaders {
					if v := resp.Header.Get(name); v != "" {
						return nil, ErrUnsupportedHeader
					}
				}
				return resp, nil
			case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				resp.Body.Close()
				timer.Reset(delay)
				delay = time.Duration(math.Min(
					float64(delay)*math.Phi,
					maxDelayBetweenRequests,
				))
				continue
			default:
				return nil, newErrQueryFailedFromResponse(resp)
			}
		}
	}
}

// ErrQueryFailed indicates that a query to Trino failed.
type ErrQueryFailed struct {
	StatusCode int
	Reason     error
}

// Error implements the error interface.
func (e *ErrQueryFailed) Error() string {
	return fmt.Sprintf("trino: query failed (%d %s): %q",
		e.StatusCode, http.StatusText(e.StatusCode), e.Reason)
}

// Unwrap implements the unwrap interface.
func (e *ErrQueryFailed) Unwrap() error {
	return e.Reason
}

func newErrQueryFailedFromResponse(resp *http.Response) *ErrQueryFailed {
	const maxBytes = 8 * 1024
	defer resp.Body.Close()
	qf := &ErrQueryFailed{StatusCode: resp.StatusCode}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		qf.Reason = err
		return qf
	}
	reason := string(b)
	if resp.ContentLength > maxBytes {
		reason += "..."
	}
	qf.Reason = errors.New(reason)
	return qf
}

type driverStmt struct {
	conn                          *Conn
	query                         string
	user                          string
	nextURIs                      chan string
	httpResponses                 chan *http.Response
	queryResponses                chan queryResponse
	statsCh                       chan QueryProgressInfo
	usingSpooledProtocol          bool
	spoolingMaxOutOfOrderSegments int
	spoolingWorkerCount           int
	spooledSegmentsMetadata       chan spooledMetadata
	spooledSegmentsToDecode       chan segmentToDecode
	decodedSegments               chan decodedSegment
	segmentsToProccess            chan segmentToProccess
	waitSegmentDecodersWorkers    sync.WaitGroup
	waitDownloadSegmentsWorkers   sync.WaitGroup
	cancelDownloadWorkers         context.CancelFunc
	cancelDecodersWorkers         context.CancelFunc
	spoolingRowsChannel           chan []queryData
	spoolingProcesserDone         chan struct{}
	segmentThrottleCh             chan struct{}
	errors                        chan error
	doneCh                        chan struct{}
	segmentDispatcherDoneCh       chan struct{}
}

type segmentToDecode struct {
	segmentIndex int
	encoding     string
	data         []byte
	metadata     segmentMetadata
}

type decodedSegment struct {
	rowOffset int64
	queryData []queryData
}

var (
	_ driver.Stmt              = &driverStmt{}
	_ driver.StmtQueryContext  = &driverStmt{}
	_ driver.StmtExecContext   = &driverStmt{}
	_ driver.NamedValueChecker = &driverStmt{}
)

// Close closes statement just before releasing connection
func (st *driverStmt) Close() error {
	if st.doneCh == nil {
		return nil
	}
	close(st.doneCh)
	if st.statsCh != nil {
		<-st.statsCh
		st.statsCh = nil
	}
	go func() {
		// drain errors chan to allow goroutines to write to it
		for range st.errors {
		}
	}()

	for range st.queryResponses {
	}
	for range st.httpResponses {
	}

	if st.cancelDownloadWorkers != nil {
		st.cancelDownloadWorkers()
	}

	if st.cancelDecodersWorkers != nil {
		st.cancelDecodersWorkers()
	}

	if st.spoolingRowsChannel != nil {
		for range st.spoolingRowsChannel {
		}
	}

	if st.decodedSegments != nil {
		for range st.decodedSegments {
		}
	}

	if st.spooledSegmentsToDecode != nil {
		for range st.spooledSegmentsToDecode {
		}
	}

	if st.spooledSegmentsMetadata != nil {
		for range st.spooledSegmentsMetadata {
		}
	}

	if st.segmentsToProccess != nil {
		for range st.segmentsToProccess {
		}
	}

	st.waitDownloadSegmentsWorkers.Wait()

	st.waitSegmentDecodersWorkers.Wait()

	close(st.nextURIs)
	close(st.errors)

	st.doneCh = nil
	st.cancelDownloadWorkers = nil
	st.spooledSegmentsMetadata = nil
	st.spooledSegmentsToDecode = nil
	st.cancelDecodersWorkers = nil
	st.segmentsToProccess = nil
	st.decodedSegments = nil
	st.spoolingRowsChannel = nil

	return nil
}

func (st *driverStmt) NumInput() int {
	return -1
}

func (st *driverStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, driver.ErrSkip
}

func (st *driverStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	sr, err := st.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	rows := &driverRows{
		ctx:          ctx,
		stmt:         st,
		queryID:      sr.ID,
		nextURI:      sr.NextURI,
		rowsAffected: sr.UpdateCount,
		statsCh:      st.statsCh,
		doneCh:       st.doneCh,
	}
	// consume all results, if there are any
	for err == nil {
		err = rows.fetch()
	}

	if err != nil && err != io.EOF {
		return nil, err
	}
	return rows, nil
}

func (st *driverStmt) CheckNamedValue(arg *driver.NamedValue) error {
	switch arg.Value.(type) {
	case nil:
		return nil
	case Numeric, trinoDate, trinoTime, trinoTimeTz, trinoTimestamp, time.Duration:
		return nil
	default:
		{
			if reflect.TypeOf(arg.Value).Kind() == reflect.Slice {
				return nil
			}

			if arg.Name == trinoProgressCallbackParam {
				return nil
			}
			if arg.Name == trinoProgressCallbackPeriodParam {
				return nil
			}
		}
	}

	return driver.ErrSkip
}

type stmtResponse struct {
	ID          string    `json:"id"`
	InfoURI     string    `json:"infoUri"`
	NextURI     string    `json:"nextUri"`
	Stats       stmtStats `json:"stats"`
	Error       ErrTrino  `json:"error"`
	UpdateType  string    `json:"updateType"`
	UpdateCount int64     `json:"updateCount"`
}

type stmtStats struct {
	State                string      `json:"state"`
	Scheduled            bool        `json:"scheduled"`
	Nodes                int         `json:"nodes"`
	TotalSplits          int         `json:"totalSplits"`
	QueuesSplits         int         `json:"queuedSplits"`
	RunningSplits        int         `json:"runningSplits"`
	CompletedSplits      int         `json:"completedSplits"`
	UserTimeMillis       int         `json:"userTimeMillis"`
	CPUTimeMillis        int64       `json:"cpuTimeMillis"`
	WallTimeMillis       int64       `json:"wallTimeMillis"`
	QueuedTimeMillis     int64       `json:"queuedTimeMillis"`
	ElapsedTimeMillis    int64       `json:"elapsedTimeMillis"`
	ProcessedRows        int64       `json:"processedRows"`
	ProcessedBytes       int64       `json:"processedBytes"`
	PhysicalInputBytes   int64       `json:"physicalInputBytes"`
	PhysicalWrittenBytes int64       `json:"physicalWrittenBytes"`
	PeakMemoryBytes      int64       `json:"peakMemoryBytes"`
	SpilledBytes         int64       `json:"spilledBytes"`
	RootStage            stmtStage   `json:"rootStage"`
	ProgressPercentage   jsonFloat64 `json:"progressPercentage"`
	RunningPercentage    jsonFloat64 `json:"runningPercentage"`
}

type ErrTrino struct {
	Message       string        `json:"message"`
	SqlState      string        `json:"sqlState"`
	ErrorCode     int           `json:"errorCode"`
	ErrorName     string        `json:"errorName"`
	ErrorType     string        `json:"errorType"`
	ErrorLocation ErrorLocation `json:"errorLocation"`
	FailureInfo   FailureInfo   `json:"failureInfo"`
}

func (i ErrTrino) Error() string {
	return i.ErrorType + ": " + i.Message
}

type ErrorLocation struct {
	LineNumber   int `json:"lineNumber"`
	ColumnNumber int `json:"columnNumber"`
}

type FailureInfo struct {
	Type          string        `json:"type"`
	Message       string        `json:"message"`
	Cause         *FailureInfo  `json:"cause"`
	Suppressed    []FailureInfo `json:"suppressed"`
	Stack         []string      `json:"stack"`
	ErrorInfo     ErrorInfo     `json:"errorInfo"`
	ErrorLocation ErrorLocation `json:"errorLocation"`
}

type ErrorInfo struct {
	Code int    `json:"code"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (i ErrorInfo) Error() string {
	return fmt.Sprintf("%s: %s (%d)", i.Type, i.Name, i.Code)
}

type stmtStage struct {
	StageID         string      `json:"stageId"`
	State           string      `json:"state"`
	Done            bool        `json:"done"`
	Nodes           int         `json:"nodes"`
	TotalSplits     int         `json:"totalSplits"`
	QueuedSplits    int         `json:"queuedSplits"`
	RunningSplits   int         `json:"runningSplits"`
	CompletedSplits int         `json:"completedSplits"`
	UserTimeMillis  int         `json:"userTimeMillis"`
	CPUTimeMillis   int         `json:"cpuTimeMillis"`
	WallTimeMillis  int         `json:"wallTimeMillis"`
	ProcessedRows   int         `json:"processedRows"`
	ProcessedBytes  int         `json:"processedBytes"`
	SubStages       []stmtStage `json:"subStages"`
}

type jsonFloat64 float64

func (f *jsonFloat64) UnmarshalJSON(data []byte) error {
	var v float64
	err := json.Unmarshal(data, &v)
	if err != nil {
		var jsonErr *json.UnmarshalTypeError
		if errors.As(err, &jsonErr) {
			if f != nil {
				*f = 0
			}
			return nil
		}
		return err
	}
	p := (*float64)(f)
	*p = v
	return nil
}

var _ json.Unmarshaler = new(jsonFloat64)

func (st *driverStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, driver.ErrSkip
}

func (st *driverStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	sr, err := st.exec(ctx, args)
	if err != nil {
		return nil, err
	}
	rows := &driverRows{
		ctx:     ctx,
		stmt:    st,
		queryID: sr.ID,
		nextURI: sr.NextURI,
		statsCh: st.statsCh,
		doneCh:  st.doneCh,
	}
	if err = rows.fetch(); err != nil && err != io.EOF {
		return nil, err
	}
	return rows, nil
}

func (st *driverStmt) exec(ctx context.Context, args []driver.NamedValue) (*stmtResponse, error) {
	query := st.query
	hs := make(http.Header)
	// Ensure the server returns timestamps preserving their precision, without truncating them to timestamp(3).
	hs.Add("X-Trino-Client-Capabilities", "PARAMETRIC_DATETIME")

	if len(args) > 0 {
		var ss []string
		for _, arg := range args {
			if arg.Name == trinoProgressCallbackParam {
				st.conn.progressUpdater = arg.Value.(ProgressUpdater)
				continue
			}
			if arg.Name == trinoProgressCallbackPeriodParam {
				st.conn.progressUpdaterPeriod.Period = arg.Value.(time.Duration)
				continue
			}

			if st.conn.forwardAuthorizationHeader && arg.Name == accessTokenConfig {
				token := arg.Value.(string)
				hs.Add(authorizationHeader, getAuthorization(token))
				continue
			}

			if arg.Name == trinoEncoding {
				hs.Add(trinoQueryDataEncodingHeader, arg.Value.(string))
				continue
			}

			if arg.Name == trinoSpoolingWorkerCount {
				numberOfWorkers, err := strconv.Atoi(arg.Value.(string))
				if err != nil {
					return nil, err
				}
				st.spoolingWorkerCount = numberOfWorkers
				continue
			}

			if arg.Name == trinoMaxOutOfOrdersSegments {
				maxSegmentsOutOfOrder, err := strconv.Atoi(arg.Value.(string))
				if err != nil {
					return nil, err
				}
				st.spoolingMaxOutOfOrderSegments = maxSegmentsOutOfOrder
				continue
			}

			s, err := Serial(arg.Value)
			if err != nil {
				return nil, err
			}

			if strings.HasPrefix(arg.Name, trinoHeaderPrefix) {
				headerValue := arg.Value.(string)

				if arg.Name == trinoUserHeader {
					st.user = headerValue
				}

				hs.Add(arg.Name, headerValue)
			} else {
				if st.conn.useExplicitPrepare && hs.Get(preparedStatementHeader) == "" {
					for _, v := range st.conn.httpHeaders.Values(preparedStatementHeader) {
						hs.Add(preparedStatementHeader, v)
					}
					hs.Add(preparedStatementHeader, preparedStatementName+"="+url.QueryEscape(st.query))
				}
				ss = append(ss, s)
			}
		}
		if (st.conn.progressUpdater != nil && st.conn.progressUpdaterPeriod.Period == 0) || (st.conn.progressUpdater == nil && st.conn.progressUpdaterPeriod.Period > 0) {
			return nil, ErrInvalidProgressCallbackHeader
		}
		if len(ss) > 0 {
			if st.conn.useExplicitPrepare {
				query = "EXECUTE " + preparedStatementName + " USING " + strings.Join(ss, ", ")
			} else {
				query = "EXECUTE IMMEDIATE " + formatStringLiteral(st.query) + " USING " + strings.Join(ss, ", ")
			}
		}
	}

	if st.spoolingWorkerCount > st.spoolingMaxOutOfOrderSegments {
		return nil, fmt.Errorf("spooling worker cannot be greater than max out of order segments allowed. spooling workers: %d, allowed out of order segments: %d", st.spoolingWorkerCount, st.spoolingMaxOutOfOrderSegments)
	}

	if hs.Get(trinoQueryDataEncodingHeader) == "" {
		hs.Add(trinoQueryDataEncodingHeader, defaulttrinoEncoding)
	}

	var cancel context.CancelFunc = func() {}
	if st.conn.queryTimeout != nil {
		ctx, cancel = context.WithTimeout(ctx, *st.conn.queryTimeout)
	} else if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, DefaultQueryTimeout)
	}

	req, err := st.conn.newRequest(ctx, "POST", st.conn.baseURL+"/v1/statement", strings.NewReader(query), hs)
	if err != nil {
		cancel()
		return nil, err
	}

	resp, err := st.conn.roundTrip(ctx, req)
	if err != nil {
		cancel()
		return nil, err
	}

	defer resp.Body.Close()
	var sr stmtResponse
	d := json.NewDecoder(resp.Body)
	d.UseNumber()
	err = d.Decode(&sr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("trino: %w", err)
	}

	st.doneCh = make(chan struct{})
	st.nextURIs = make(chan string)
	st.httpResponses = make(chan *http.Response)
	st.queryResponses = make(chan queryResponse)
	st.errors = make(chan error)
	go func() {
		defer close(st.httpResponses)
		for {
			select {
			case nextURI := <-st.nextURIs:
				if nextURI == "" {
					return
				}
				hs := make(http.Header)
				hs.Add(trinoUserHeader, st.user)
				req, err := st.conn.newRequest(ctx, "GET", nextURI, nil, hs)
				if err != nil {
					if ctx.Err() == context.Canceled {
						st.errors <- context.Canceled
						return
					}
					st.errors <- err
					return
				}
				resp, err := st.conn.roundTrip(ctx, req)
				if err != nil {
					if ctx.Err() == context.Canceled {
						st.errors <- context.Canceled
						return
					}
					st.errors <- err
					return
				}
				select {
				case st.httpResponses <- resp:
				case <-st.doneCh:
					return
				}
			case <-st.doneCh:
				return
			}
		}
	}()
	go func() {
		defer close(st.queryResponses)
		defer cancel()
		for {
			select {
			case resp := <-st.httpResponses:
				if resp == nil {
					return
				}
				var qresp queryResponse
				d := json.NewDecoder(resp.Body)
				d.UseNumber()
				err = d.Decode(&qresp)
				if err != nil {
					st.errors <- fmt.Errorf("trino: %w", err)
					return
				}
				err = resp.Body.Close()
				if err != nil {
					st.errors <- err
					return
				}
				err = handleResponseError(resp.StatusCode, qresp.Error)
				if err != nil {
					st.errors <- err
					return
				}
				select {
				case st.nextURIs <- qresp.NextURI:
				case <-st.doneCh:
					return
				}
				select {
				case st.queryResponses <- qresp:
				case <-st.doneCh:
					return
				}
			case <-st.doneCh:
				return
			}
		}
	}()
	st.nextURIs <- sr.NextURI
	if st.conn.progressUpdater != nil {
		st.statsCh = make(chan QueryProgressInfo)

		// progress updater go func
		go func() {
			for {
				select {
				case stats := <-st.statsCh:
					st.conn.progressUpdater.Update(stats)
				case <-st.doneCh:
					close(st.statsCh)
					return
				}
			}
		}()

		// initial progress callback call
		srStats := QueryProgressInfo{
			QueryId:    sr.ID,
			QueryStats: sr.Stats,
		}
		select {
		case st.statsCh <- srStats:
		default:
			// ignore when can't send stats
		}
		st.conn.progressUpdaterPeriod.LastCallbackTime = time.Now()
		st.conn.progressUpdaterPeriod.LastQueryState = sr.Stats.State
	}
	return &sr, handleResponseError(resp.StatusCode, sr.Error)
}

type SegmentFetcher struct {
	ctx             context.Context
	httpClient      http.Client
	spooledMetadata spooledMetadata
}

func (sf *SegmentFetcher) roundTrip(req *http.Request) (*http.Response, error) {
	delay := 200 * time.Millisecond
	const maxRetries = 5

	retries := 0
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			resp, err := sf.httpClient.Do(req)
			if err != nil {
				var netErr net.Error

				if errors.As(err, &netErr) && netErr.Timeout() {
					retries++
					if retries > maxRetries {
						return nil, &ErrQueryFailed{Reason: fmt.Errorf("max retries reached: %w", err)}
					}
					delay = time.Duration(float64(delay) * math.Phi)
					timer.Reset(delay)
					continue
				}

				return nil, &ErrQueryFailed{Reason: err}
			}

			switch resp.StatusCode {
			case http.StatusOK:
				return resp, nil

			case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				resp.Body.Close()
				retries++
				if retries > maxRetries {
					return nil, &ErrQueryFailed{Reason: fmt.Errorf("max retries reached for status code %d", resp.StatusCode)}
				}
				delay = time.Duration(float64(delay) * math.Phi)
				timer.Reset(delay)
				continue

			default:
				return nil, newErrQueryFailedFromResponse(resp)
			}
		}
	}
}

func (sf *SegmentFetcher) fetchSegment() ([]byte, error) {
	req, err := http.NewRequestWithContext(sf.ctx, "GET", sf.spooledMetadata.uri, nil)
	if err != nil {
		return nil, err
	}

	for k, v := range sf.spooledMetadata.headers {
		headerSlice, ok := v.([]interface{})
		if !ok {
			return nil, fmt.Errorf("unsupported header type %T", v)
		}

		if len(headerSlice) == 0 {
			continue
		}

		if len(headerSlice) > 1 {
			return nil, fmt.Errorf("multiple values for header %s", k)
		}

		header, ok := headerSlice[0].(string)
		if !ok {
			return nil, fmt.Errorf("unsupported header value type %T", headerSlice[0])
		}
		req.Header.Add(k, header)
	}

	resp, err := sf.roundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("error fetching segment from uri '%s': %v", sf.spooledMetadata.uri, err)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %v", err)
	}

	//acknowledge the segment read
	go func() {
		// TODO: handle ack erros
		ackReq, err := http.NewRequestWithContext(sf.ctx, "GET", sf.spooledMetadata.ackUri, nil)
		if err != nil {
			return
		}

		for k, values := range req.Header {
			for _, v := range values {
				ackReq.Header.Add(k, v)
			}
		}

		resp, err := sf.httpClient.Do(ackReq)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()

	return data, nil
}

func formatStringLiteral(query string) string {
	return "'" + strings.ReplaceAll(query, "'", "''") + "'"
}

type driverRows struct {
	ctx     context.Context
	stmt    *driverStmt
	queryID string
	nextURI string

	err          error
	rowindex     int
	columns      []string
	coltype      []*typeConverter
	data         []queryData
	rowsAffected int64

	statsCh chan QueryProgressInfo
	doneCh  chan struct{}
}

var _ driver.Rows = &driverRows{}
var _ driver.Result = &driverRows{}
var _ driver.RowsColumnTypeScanType = &driverRows{}
var _ driver.RowsColumnTypeDatabaseTypeName = &driverRows{}
var _ driver.RowsColumnTypeLength = &driverRows{}
var _ driver.RowsColumnTypePrecisionScale = &driverRows{}

// Close closes the rows iterator.
func (qr *driverRows) Close() error {
	if qr.err == sql.ErrNoRows || qr.err == io.EOF {
		return nil
	}
	qr.err = io.EOF
	hs := make(http.Header)
	if qr.stmt.user != "" {
		hs.Add(trinoUserHeader, qr.stmt.user)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(qr.ctx), DefaultCancelQueryTimeout)
	defer cancel()
	req, err := qr.stmt.conn.newRequest(ctx, "DELETE", qr.stmt.conn.baseURL+"/v1/query/"+url.PathEscape(qr.queryID), nil, hs)
	if err != nil {
		return err
	}
	resp, err := qr.stmt.conn.roundTrip(ctx, req)
	if err != nil {
		qferr, ok := err.(*ErrQueryFailed)
		if ok && qferr.StatusCode == http.StatusNoContent {
			qr.nextURI = ""
			return nil
		}
		return err
	}
	resp.Body.Close()
	return qr.err
}

// Columns returns the names of the columns.
func (qr *driverRows) Columns() []string {
	if qr.err != nil {
		return []string{}
	}
	if qr.columns == nil {
		if err := qr.fetch(); err != nil && err != io.EOF {
			qr.err = err
			return []string{}
		}
	}
	return qr.columns
}

func (qr *driverRows) ColumnTypeDatabaseTypeName(index int) string {
	typeName := qr.coltype[index].parsedType[0]
	if typeName == "map" || typeName == "array" || typeName == "row" {
		typeName = qr.coltype[index].typeName
	}
	return strings.ToUpper(typeName)
}

func (qr *driverRows) ColumnTypeScanType(index int) reflect.Type {
	return qr.coltype[index].scanType
}

func (qr *driverRows) ColumnTypeLength(index int) (int64, bool) {
	return qr.coltype[index].size.value, qr.coltype[index].size.hasValue
}

func (qr *driverRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	return qr.coltype[index].precision.value, qr.coltype[index].scale.value, qr.coltype[index].precision.hasValue
}

// Next is called to populate the next row of data into
// the provided slice. The provided slice will be the same
// size as the Columns() are wide.
//
// Next should return io.EOF when there are no more rows.
func (qr *driverRows) Next(dest []driver.Value) error {
	if qr.err != nil {
		return qr.err
	}
	if !qr.stmt.usingSpooledProtocol && (qr.columns == nil || qr.rowindex >= len(qr.data)) {
		if qr.nextURI == "" {
			qr.err = io.EOF
			return qr.err
		}
		if err := qr.fetch(); err != nil {
			qr.err = err
			return err
		}
	} else if qr.stmt.usingSpooledProtocol && (qr.rowindex >= len(qr.data) || qr.data == nil) {
		var ok bool
		select {
		// The spoolingRowsChannel is initialized in startSpoolingProtocolWorkers,
		// which is called by fetch() when the first query response indicates
		// the spooling protocol (i.e., the response contains segments).
		// At that point, usingSpooledProtocol is set to true and the channel is created.
		case qr.data, ok = <-qr.stmt.spoolingRowsChannel:
			if !ok {
				qr.err = io.EOF
				return qr.err
			}

			qr.rowindex = 0

		case err := <-qr.stmt.errors:
			if err == nil {
				// Channel was closed, which means the statement
				// or rows were closed.
				qr.err = io.EOF
				return qr.err
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				qr.Close()
			}
			qr.stmt.cancelDecodersWorkers()
			qr.stmt.cancelDownloadWorkers()
			qr.err = err
			return qr.err
		}
	}

	return qr.next(dest)
}

func (qr *driverRows) next(dest []driver.Value) error {
	if len(qr.coltype) == 0 {
		qr.err = sql.ErrNoRows
		return qr.err
	}
	for i, v := range qr.coltype {
		if i > len(dest)-1 {
			break
		}
		vv, err := v.ConvertValue(qr.data[qr.rowindex][i])
		if err != nil {
			qr.err = err
			return err
		}
		dest[i] = vv
	}
	qr.rowindex++
	return nil
}

// LastInsertId returns the database's auto-generated ID
// after, for example, an INSERT into a table with primary
// key.
func (qr driverRows) LastInsertId() (int64, error) {
	return 0, ErrOperationNotSupported
}

// RowsAffected returns the number of rows affected by the query.
func (qr driverRows) RowsAffected() (int64, error) {
	return qr.rowsAffected, nil
}

type queryResponse struct {
	ID               string        `json:"id"`
	InfoURI          string        `json:"infoUri"`
	PartialCancelURI string        `json:"partialCancelUri"`
	NextURI          string        `json:"nextUri"`
	Columns          []queryColumn `json:"columns"`
	Data             interface{}   `json:"data"`
	Stats            stmtStats     `json:"stats"`
	Error            ErrTrino      `json:"error"`
	UpdateType       string        `json:"updateType"`
	UpdateCount      int64         `json:"updateCount"`
}

type segmentMetadata struct {
	rowOffset        int64
	rowsCount        int64
	segmentSize      int64
	uncompressedSize int64
}

type spooledMetadata struct {
	uri      string
	ackUri   string
	encoding string
	headers  map[string]interface{}
	metadata segmentMetadata
}

func parseSpooledMetadata(segment map[string]interface{}, segmentIndex int, segmentMetadata segmentMetadata, encoding string) (spooledMetadata, error) {
	result := spooledMetadata{
		metadata: segmentMetadata,
		encoding: encoding,
		headers:  make(map[string]interface{}),
	}

	var ok bool
	result.uri, ok = segment["uri"].(string)
	if !ok || result.uri == "" {
		return spooledMetadata{}, fmt.Errorf("missing or invalid 'uri' field in spooled segment at index %d", segmentIndex)
	}

	result.ackUri, ok = segment["ackUri"].(string)
	if !ok || result.ackUri == "" {
		return spooledMetadata{}, fmt.Errorf("missing or invalid 'ackUri' field in spooled segment at index %d", segmentIndex)
	}

	if rawHeaders, exists := segment["headers"]; exists {
		result.headers, ok = rawHeaders.(map[string]interface{})
		if !ok {
			return spooledMetadata{}, fmt.Errorf("invalid 'headers' field in spooled segment at index %d: expected map[string]interface{}", segmentIndex)
		}
	}

	return result, nil
}

func parseSegmentMetadata(metadata map[string]interface{}) (segmentMetadata, error) {
	result := segmentMetadata{
		rowOffset:        0,
		rowsCount:        0,
		segmentSize:      0,
		uncompressedSize: 0,
	}

	var err error
	// Mandatory field
	if result.rowOffset, err = getInt64(metadata, "rowOffset"); err != nil {
		return segmentMetadata{}, err
	}

	// Mandatory field
	if result.segmentSize, err = getInt64(metadata, "segmentSize"); err != nil {
		return segmentMetadata{}, err
	}

	if result.uncompressedSize, err = getOptionalInt64(metadata, "uncompressedSize"); err != nil {
		return segmentMetadata{}, err
	}

	// Bug: rowsCount was wrongly not enforced as a mandatory field on Trino response. Fixed on 475 release
	if result.rowsCount, err = getOptionalInt64(metadata, "rowsCount"); err != nil {
		return segmentMetadata{}, err
	}

	return result, nil
}

func getInt64(metadata map[string]interface{}, key string) (int64, error) {
	val, exists := metadata[key]
	if !exists {
		return 0, fmt.Errorf("%s is missing in segment metadata", key)
	}

	return parseInt64(val, key)
}

func getOptionalInt64(metadata map[string]interface{}, key string) (int64, error) {
	val, exists := metadata[key]
	if !exists {
		return 0, nil
	}

	return parseInt64(val, key)
}

func parseInt64(val interface{}, key string) (int64, error) {
	num, ok := val.(json.Number)
	if !ok {
		return 0, fmt.Errorf("invalid type for %s in segment metadata, expected json.Number, got %T", key, val)
	}

	n, err := num.Int64()
	if err != nil {
		return 0, fmt.Errorf("error converting %s to int64: %v", key, err)
	}

	return n, nil
}

func decodeSegment(data []byte, encoding string, metadata segmentMetadata) ([]queryData, error) {
	if int64(len(data)) != metadata.segmentSize {
		return nil, fmt.Errorf("segment size mismatch: expected %d bytes, got %d bytes", metadata.segmentSize, len(data))
	}

	decompressedSegment, err := decompressSegment(data, encoding, metadata)
	if err != nil {
		return nil, err
	}

	var queryDataList = make([]queryData, metadata.rowsCount)
	decoder := json.NewDecoder(bytes.NewReader(decompressedSegment))
	decoder.UseNumber()
	err = decoder.Decode(&queryDataList)
	if err != nil {
		return nil, fmt.Errorf("failed to decode segment into JSON at rowOffset %d: %v", metadata.rowOffset, err)
	}

	return queryDataList, nil
}

func decompressSegment(data []byte, encoding string, metadata segmentMetadata) ([]byte, error) {
	if metadata.uncompressedSize == 0 {
		return data, nil
	}

	var decompressedData []byte
	switch encoding {
	case "json+zstd":
		zstdReader, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("error creating zstd reader: %w", err)
		}
		defer zstdReader.Close()
		decompressedData, err = io.ReadAll(zstdReader)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress zstd segment at rowOffset %d: %v", metadata.rowOffset, err)
		}
	case "json+lz4":
		decompressedData = make([]byte, metadata.uncompressedSize)

		n, err := lz4.UncompressBlock(data, decompressedData)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress LZ4 segment at rowOffset %d: %v", metadata.rowOffset, err)
		}

		decompressedData = decompressedData[:n]
	default:
		return nil, fmt.Errorf("unsupported segment encoder: %s", encoding)
	}

	if int64(len(decompressedData)) != metadata.uncompressedSize {
		return nil, fmt.Errorf("decompressed size mismatch: expected %d bytes, got %d bytes", metadata.uncompressedSize, len(decompressedData))
	}

	return decompressedData, nil
}

type queryColumn struct {
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	TypeSignature typeSignature `json:"typeSignature"`
}

type queryData []interface{}

type namedTypeSignature struct {
	FieldName     rowFieldName  `json:"fieldName"`
	TypeSignature typeSignature `json:"typeSignature"`
}

type rowFieldName struct {
	Name string `json:"name"`
}

type typeSignature struct {
	RawType   string         `json:"rawType"`
	Arguments []typeArgument `json:"arguments"`
}

type typeKind string

const (
	KIND_TYPE       = typeKind("TYPE")
	KIND_NAMED_TYPE = typeKind("NAMED_TYPE")
	KIND_LONG       = typeKind("LONG")
	KIND_VARIABLE   = typeKind("VARIABLE")
)

type typeArgument struct {
	// Kind determines if the typeSignature, namedTypeSignature, or long field has a value
	Kind  typeKind        `json:"kind"`
	Value json.RawMessage `json:"value"`
	// typeSignature decoded from Value when Kind is TYPE
	typeSignature typeSignature
	// namedTypeSignature decoded from Value when Kind is NAMED_TYPE
	namedTypeSignature namedTypeSignature
	// long decoded from Value when Kind is LONG
	long int64
}

func handleResponseError(status int, respErr ErrTrino) error {
	switch respErr.ErrorName {
	case "":
		return nil
	case "USER_CANCELLED":
		return ErrQueryCancelled
	default:
		return &ErrQueryFailed{
			StatusCode: status,
			Reason:     &respErr,
		}
	}
}

func (qr *driverRows) startOrderedSegmentStreamer() {
	go func() {
		defer close(qr.stmt.spoolingRowsChannel)
		defer close(qr.stmt.spoolingProcesserDone)

		consumed := 0
		buffer := make([]decodedSegment, 0, qr.stmt.spoolingMaxOutOfOrderSegments)
		var nextExpectedOffset int64 = 0

		for {
			select {
			case segment, ok := <-qr.stmt.decodedSegments:
				if !ok {
					return
				}

				buffer = append(buffer, segment)

				if nextExpectedOffset != segment.rowOffset {
					if len(buffer) >= qr.stmt.spoolingMaxOutOfOrderSegments {
						qr.stmt.errors <- fmt.Errorf(
							"all %d out-of-order segments buffered (limit: %d). This indicates a bug or inconsistency in the segments metadata response (e.g., missing, duplicate, or misordered segments, or row offsets not matching the expected sequence)",
							len(buffer), qr.stmt.spoolingMaxOutOfOrderSegments)
					}

					continue
				}

				consumed = 0
				slices.SortFunc(buffer, func(a, b decodedSegment) int {
					if a.rowOffset < b.rowOffset {
						return -1
					}
					if a.rowOffset > b.rowOffset {
						return 1
					}
					return 0
				})

				for consumed < len(buffer) && buffer[consumed].rowOffset == nextExpectedOffset {
					select {
					case qr.stmt.spoolingRowsChannel <- buffer[consumed].queryData:
					case <-qr.doneCh:
						return
					}

					// release reserved slot
					select {
					case <-qr.stmt.segmentThrottleCh:
					case <-qr.doneCh:
						return
					}

					nextExpectedOffset += int64(len(buffer[consumed].queryData))
					consumed++
				}

				copy(buffer[0:], buffer[consumed:])
				buffer = buffer[:len(buffer)-consumed]

			case <-qr.doneCh:
				return
			}
		}
	}()
}

func (qr *driverRows) fetch() error {
	var qresp queryResponse
	var err error
	for {
		select {
		case qresp = <-qr.stmt.queryResponses:
			if qresp.ID == "" {
				return io.EOF
			}

			err = qr.initColumns(&qresp)
			if err != nil {
				return err
			}

			qr.rowindex = 0
			switch data := qresp.Data.(type) {
			case []interface{}:
				// direct protocol
				qr.data = make([]queryData, len(data))
				for i, item := range data {
					if row, ok := item.([]interface{}); ok {
						qr.data[i] = row
					} else {
						return fmt.Errorf("unexpected data type for row at index %d: expected []interface{}, got %T", i, item)
					}
				}
			case map[string]interface{}:
				// spooling protocol
				qr.stmt.startSpoolingProtocolWorkers(qr.ctx)
				qr.startOrderedSegmentStreamer()

				err := qr.queueSpoolingSegments(data)
				qr.proccessSpollingSegments()

				return err
			case nil:
				qr.data = nil
			}
			qr.rowsAffected = qresp.UpdateCount
			qr.scheduleProgressUpdate(qresp.ID, qresp.Stats)
			if len(qr.data) != 0 {
				return nil
			}
		case err = <-qr.stmt.errors:
			if err == nil {
				// Channel was closed, which means the statement
				// or rows were closed.
				err = io.EOF
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				qr.Close()
			}
			qr.err = err
			return err
		}
	}
}

func (st *driverStmt) startSpoolingProtocolWorkers(ctx context.Context) {
	st.usingSpooledProtocol = true

	if st.spoolingWorkerCount == 0 {
		st.spoolingWorkerCount = defaultSpoolingDownloadWorkers
	}

	if st.spoolingMaxOutOfOrderSegments == 0 {
		st.spoolingMaxOutOfOrderSegments = defaultallowedOutOfOrder
	}

	downloadSegmentsCtx, cancelDownloadWorkers := context.WithCancel(context.WithoutCancel(ctx))
	st.cancelDownloadWorkers = cancelDownloadWorkers
	decodeSegmentCtx, cancelDecodersWorkers := context.WithCancel(context.WithoutCancel(ctx))
	st.cancelDecodersWorkers = cancelDecodersWorkers

	st.segmentsToProccess = make(chan segmentToProccess, 1000)
	st.spooledSegmentsMetadata = make(chan spooledMetadata, st.spoolingMaxOutOfOrderSegments)
	st.spooledSegmentsToDecode = make(chan segmentToDecode, st.spoolingMaxOutOfOrderSegments)
	st.spoolingRowsChannel = make(chan []queryData)
	st.spoolingProcesserDone = make(chan struct{})
	st.segmentDispatcherDoneCh = make(chan struct{})
	st.segmentThrottleCh = make(chan struct{}, st.spoolingMaxOutOfOrderSegments)
	st.decodedSegments = make(chan decodedSegment)

	st.startSegmentDispatcher()
	st.startDownloadSegmentsWorkers(downloadSegmentsCtx)
	st.startSegmentsDecodersWorkers(decodeSegmentCtx)
}

func (st *driverStmt) startSegmentDispatcher() {
	go func() {
		defer close(st.segmentDispatcherDoneCh)
		defer close(st.segmentThrottleCh)
		for {
			select {
			case segmentToProccess, ok := <-st.segmentsToProccess:
				if !ok {
					return
				}

				// segmentThrottleCh blocks if there are too many out-of-order segments.
				// Once all currently downloaded segments are downloaded, decoded,
				// and can be ordered, this channel will be drained.
				select {
				case st.segmentThrottleCh <- struct{}{}:
				case <-st.doneCh:
					return
				}

				segmentMetadata, exists := segmentToProccess.segment["metadata"]
				if !exists {
					st.errors <- fmt.Errorf("metadata is missing in segment at index %d", segmentToProccess.segmentIndex)
				}

				typedMetadata, ok := segmentMetadata.(map[string]interface{})
				if !ok {
					st.errors <- fmt.Errorf("metadata is invalid or cannot be parsed as map[string]interface{} in segment at index %d", segmentToProccess.segmentIndex)
				}

				metadata, err := parseSegmentMetadata(typedMetadata)

				if err != nil {
					st.errors <- err
				}
				switch segmentToProccess.segment["type"] {
				case "inline":
					decodedBytes, err := base64.StdEncoding.DecodeString(segmentToProccess.segment["data"].(string))

					if err != nil {
						st.errors <- fmt.Errorf("error decoding base64 data in inline segment at index %d: %v", segmentToProccess.segmentIndex, err)
					}

					st.spooledSegmentsToDecode <- segmentToDecode{
						segmentIndex: 0,
						encoding:     segmentToProccess.encoding,
						data:         decodedBytes,
						metadata:     metadata,
					}

				case "spooled":
					spooledMetadata, err := parseSpooledMetadata(segmentToProccess.segment, 0, metadata, segmentToProccess.encoding)
					if err != nil {
						st.errors <- err
					}

					st.spooledSegmentsMetadata <- spooledMetadata
				}

			case <-st.doneCh:
				return
			}
		}
	}()
}

func (st *driverStmt) startDownloadSegmentsWorkers(ctx context.Context) {
	st.waitDownloadSegmentsWorkers.Add(st.spoolingWorkerCount)
	for i := 0; i < st.spoolingWorkerCount; i++ {
		go func() {
			defer st.waitDownloadSegmentsWorkers.Done()
			for {
				select {
				case metadata, ok := <-st.spooledSegmentsMetadata:
					if !ok {
						return
					}

					segmentFetcher := &SegmentFetcher{
						ctx:             ctx,
						httpClient:      st.conn.httpClient,
						spooledMetadata: metadata,
					}

					segment, err := segmentFetcher.fetchSegment()
					if err != nil {
						st.errors <- err
						return
					}

					select {
					case st.spooledSegmentsToDecode <- segmentToDecode{
						encoding: metadata.encoding,
						data:     segment,
						metadata: metadata.metadata,
					}:
					case <-st.doneCh:
						return
					case <-ctx.Done():
						return
					}

				case <-st.doneCh:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

func (st *driverStmt) startSegmentsDecodersWorkers(ctx context.Context) {
	st.waitSegmentDecodersWorkers.Add(st.spoolingWorkerCount)
	for i := 0; i < st.spoolingWorkerCount; i++ {
		go func() {
			defer st.waitSegmentDecodersWorkers.Done()
			for {
				select {
				case segmentToDecode, ok := <-st.spooledSegmentsToDecode:

					if !ok {
						return
					}

					segment, err := decodeSegment(segmentToDecode.data, segmentToDecode.encoding, segmentToDecode.metadata)
					if err != nil {
						st.cancelDecodersWorkers()
						st.errors <- fmt.Errorf("failed to decode spooled segment at index %d: %v", segmentToDecode.segmentIndex, err)
						return
					}

					select {
					case st.decodedSegments <- decodedSegment{
						rowOffset: segmentToDecode.metadata.rowOffset,
						queryData: segment,
					}:
					case <-st.doneCh:
						return
					case <-ctx.Done():
						return
					}

				case <-st.doneCh:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

func (qr *driverRows) proccessSpollingSegments() {
	go func() {
		var qresp queryResponse
		var err error
		for {
			select {
			case qresp = <-qr.stmt.queryResponses:

				if qresp.ID == "" {
					qr.waitForAllSpoolingWorkersFinish()
					return
				}

				err = qr.initColumns(&qresp)
				if err != nil {
					qr.stmt.errors <- err
				}

				switch data := qresp.Data.(type) {
				case map[string]interface{}:
					if err := qr.queueSpoolingSegments(data); err != nil {
						qr.stmt.errors <- err
					}

				case nil:
					// do nothing: trino response without data (e.g only status information)
				default:
					qr.stmt.errors <- fmt.Errorf("unexpected data type for row at index %s: expected map[string]interface{}, got %T", qresp.ID, data)
				}
				qr.scheduleProgressUpdate(qresp.ID, qresp.Stats)
			}
		}
	}()
}

func (qr *driverRows) waitForAllSpoolingWorkersFinish() {
	close(qr.stmt.segmentsToProccess)
	<-qr.stmt.segmentDispatcherDoneCh
	close(qr.stmt.spooledSegmentsMetadata)
	qr.stmt.waitDownloadSegmentsWorkers.Wait()
	close(qr.stmt.spooledSegmentsToDecode)
	qr.stmt.waitSegmentDecodersWorkers.Wait()
	close(qr.stmt.decodedSegments)
	<-qr.stmt.spoolingProcesserDone
}

type segmentToProccess struct {
	segmentIndex int
	encoding     string
	segment      map[string]interface{}
}

func (qr *driverRows) queueSpoolingSegments(data map[string]interface{}) error {
	encoding, ok := data["encoding"].(string)
	if !ok {
		return fmt.Errorf("invalid or missing 'encoding' field on spooling protocol, expected string")
	}

	segments, ok := data["segments"].([]interface{})
	if !ok {
		return fmt.Errorf("invalid or missing 'segments' field on spooling protocol, expected []interface{}")
	}

	for segmentIndex, segment := range segments {
		segment, ok := segment.(map[string]interface{})
		if !ok {
			return fmt.Errorf("segment at index %d is invalid: expected map[string]interface{}, got %T", segmentIndex, segment)
		}

		qr.stmt.segmentsToProccess <- segmentToProccess{
			segmentIndex: segmentIndex,
			encoding:     encoding,
			segment:      segment,
		}

	}

	return nil
}

func unmarshalArguments(signature *typeSignature) error {
	for i, argument := range signature.Arguments {
		var payload interface{}
		switch argument.Kind {
		case KIND_TYPE:
			payload = &(signature.Arguments[i].typeSignature)
		case KIND_NAMED_TYPE:
			payload = &(signature.Arguments[i].namedTypeSignature)
		case KIND_LONG:
			payload = &(signature.Arguments[i].long)
		}
		err := json.Unmarshal(argument.Value, payload)
		if err != nil {
			return err
		}
		switch argument.Kind {
		case KIND_TYPE:
			err = unmarshalArguments(&(signature.Arguments[i].typeSignature))
		case KIND_NAMED_TYPE:
			err = unmarshalArguments(&(signature.Arguments[i].namedTypeSignature.TypeSignature))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (qr *driverRows) initColumns(qresp *queryResponse) error {
	if qr.columns != nil || len(qresp.Columns) == 0 {
		return nil
	}
	var err error
	for i := range qresp.Columns {
		err = unmarshalArguments(&(qresp.Columns[i].TypeSignature))
		if err != nil {
			return fmt.Errorf("error decoding column type signature: %w", err)
		}
	}
	qr.columns = make([]string, len(qresp.Columns))
	qr.coltype = make([]*typeConverter, len(qresp.Columns))
	for i, col := range qresp.Columns {
		err = unmarshalArguments(&(qresp.Columns[i].TypeSignature))
		if err != nil {
			return fmt.Errorf("error decoding column type signature: %w", err)
		}
		qr.columns[i] = col.Name
		qr.coltype[i], err = newTypeConverter(col.Type, col.TypeSignature)
		if err != nil {
			return err
		}
	}
	return nil
}

func (qr *driverRows) scheduleProgressUpdate(id string, stats stmtStats) {
	if qr.stmt.conn.progressUpdater == nil {
		return
	}

	qrStats := QueryProgressInfo{
		QueryId:    id,
		QueryStats: stats,
	}
	currentTime := time.Now()
	diff := currentTime.Sub(qr.stmt.conn.progressUpdaterPeriod.LastCallbackTime)
	period := qr.stmt.conn.progressUpdaterPeriod.Period

	// Check if period has not passed yet AND if query state did not change
	if diff < period && qr.stmt.conn.progressUpdaterPeriod.LastQueryState == qrStats.QueryStats.State {
		return
	}

	select {
	case qr.statsCh <- qrStats:
	default:
		// ignore when can't send stats
	}
	qr.stmt.conn.progressUpdaterPeriod.LastCallbackTime = currentTime
	qr.stmt.conn.progressUpdaterPeriod.LastQueryState = qrStats.QueryStats.State
}

type typeConverter struct {
	typeName   string
	parsedType []string
	scanType   reflect.Type
	precision  optionalInt64
	scale      optionalInt64
	size       optionalInt64
}

type optionalInt64 struct {
	value    int64
	hasValue bool
}

func newOptionalInt64(value int64) optionalInt64 {
	return optionalInt64{value: value, hasValue: true}
}

func newTypeConverter(typeName string, signature typeSignature) (*typeConverter, error) {
	result := &typeConverter{
		typeName:   typeName,
		parsedType: getNestedTypes([]string{}, signature),
	}
	var err error
	result.scanType, err = getScanType(result.parsedType)
	if err != nil {
		return nil, err
	}
	switch signature.RawType {
	case "char", "varchar":
		if len(signature.Arguments) > 0 {
			if signature.Arguments[0].Kind != KIND_LONG {
				return nil, ErrInvalidResponseType
			}
			result.size = newOptionalInt64(signature.Arguments[0].long)
		}
	case "decimal":
		if len(signature.Arguments) > 0 {
			if signature.Arguments[0].Kind != KIND_LONG {
				return nil, ErrInvalidResponseType
			}
			result.precision = newOptionalInt64(signature.Arguments[0].long)
		}
		if len(signature.Arguments) > 1 {
			if signature.Arguments[1].Kind != KIND_LONG {
				return nil, ErrInvalidResponseType
			}
			result.scale = newOptionalInt64(signature.Arguments[1].long)
		}
	case "time", "time with time zone", "timestamp", "timestamp with time zone":
		if len(signature.Arguments) > 0 {
			if signature.Arguments[0].Kind != KIND_LONG {
				return nil, ErrInvalidResponseType
			}
			result.precision = newOptionalInt64(signature.Arguments[0].long)
		}
	}

	return result, nil
}

func getNestedTypes(types []string, signature typeSignature) []string {
	types = append(types, signature.RawType)
	if len(signature.Arguments) == 1 {
		switch signature.Arguments[0].Kind {
		case KIND_TYPE:
			types = getNestedTypes(types, signature.Arguments[0].typeSignature)
		case KIND_NAMED_TYPE:
			types = getNestedTypes(types, signature.Arguments[0].namedTypeSignature.TypeSignature)
		}
	}
	return types
}

func getScanType(typeNames []string) (reflect.Type, error) {
	var v interface{}
	switch typeNames[0] {
	case "boolean":
		v = sql.NullBool{}
	case "json", "char", "varchar", "interval year to month", "interval day to second", "decimal", "ipaddress", "uuid", "unknown":
		v = sql.NullString{}
	case "varbinary":
		v = []byte{}
	case "tinyint", "smallint":
		v = sql.NullInt32{}
	case "integer":
		v = sql.NullInt32{}
	case "bigint":
		v = sql.NullInt64{}
	case "real", "double":
		v = sql.NullFloat64{}
	case "date", "time", "time with time zone", "timestamp", "timestamp with time zone":
		v = sql.NullTime{}
	case "map":
		v = NullMap{}
	case "array":
		if len(typeNames) <= 1 {
			return nil, ErrInvalidResponseType
		}
		switch typeNames[1] {
		case "boolean":
			v = NullSliceBool{}
		case "json", "char", "varchar", "varbinary", "interval year to month", "interval day to second", "decimal", "ipaddress", "uuid", "unknown":
			v = NullSliceString{}
		case "tinyint", "smallint", "integer", "bigint":
			v = NullSliceInt64{}
		case "real", "double":
			v = NullSliceFloat64{}
		case "date", "time", "time with time zone", "timestamp", "timestamp with time zone":
			v = NullSliceTime{}
		case "map":
			v = NullSliceMap{}
		case "array":
			if len(typeNames) <= 2 {
				return nil, ErrInvalidResponseType
			}
			switch typeNames[2] {
			case "boolean":
				v = NullSlice2Bool{}
			case "json", "char", "varchar", "varbinary", "interval year to month", "interval day to second", "decimal", "ipaddress", "uuid", "unknown":
				v = NullSlice2String{}
			case "tinyint", "smallint", "integer", "bigint":
				v = NullSlice2Int64{}
			case "real", "double":
				v = NullSlice2Float64{}
			case "date", "time", "time with time zone", "timestamp", "timestamp with time zone":
				v = NullSlice2Time{}
			case "map":
				v = NullSlice2Map{}
			case "array":
				if len(typeNames) <= 3 {
					return nil, ErrInvalidResponseType
				}
				switch typeNames[3] {
				case "boolean":
					v = NullSlice3Bool{}
				case "json", "char", "varchar", "varbinary", "interval year to month", "interval day to second", "decimal", "ipaddress", "uuid", "unknown":
					v = NullSlice3String{}
				case "tinyint", "smallint", "integer", "bigint":
					v = NullSlice3Int64{}
				case "real", "double":
					v = NullSlice3Float64{}
				case "date", "time", "time with time zone", "timestamp", "timestamp with time zone":
					v = NullSlice3Time{}
				case "map":
					v = NullSlice3Map{}
				}
				// if this is a 4 or more dimensional array, scan type will be an empty interface
			}
		}
	}
	if v == nil {
		return reflect.TypeOf(new(interface{})).Elem(), nil
	}
	return reflect.TypeOf(v), nil
}

// ConvertValue implements the driver.ValueConverter interface.
func (c *typeConverter) ConvertValue(v interface{}) (driver.Value, error) {
	switch c.parsedType[0] {
	case "boolean":
		vv, err := scanNullBool(v)
		if !vv.Valid {
			return nil, err
		}
		return vv.Bool, err
	case "json", "char", "varchar", "interval year to month", "interval day to second", "decimal", "ipaddress", "uuid", "Geometry", "SphericalGeography", "unknown":
		vv, err := scanNullString(v)
		if !vv.Valid {
			return nil, err
		}
		return vv.String, err
	case "varbinary":
		return scanNullBytes(v)
	case "tinyint", "smallint", "integer", "bigint":
		vv, err := scanNullInt64(v)
		if !vv.Valid {
			return nil, err
		}
		return vv.Int64, err
	case "real", "double":
		vv, err := scanNullFloat64(v)
		if !vv.Valid {
			return nil, err
		}
		return vv.Float64, err
	case "date", "time", "time with time zone", "timestamp", "timestamp with time zone":
		vv, err := scanNullTime(v)
		if !vv.Valid {
			return nil, err
		}
		return vv.Time, err
	case "map":
		if err := validateMap(v); err != nil {
			return nil, err
		}
		return v, nil
	case "array":
		if err := validateSlice(v); err != nil {
			return nil, err
		}
		return v, nil
	case "row":
		if err := validateSlice(v); err != nil {
			return nil, err
		}
		return v, nil
	default:
		return nil, fmt.Errorf("type not supported: %q", c.typeName)
	}
}

func validateMap(v interface{}) error {
	if v == nil {
		return nil
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return fmt.Errorf("cannot convert %v (%T) to map", v, v)
	}
	return nil
}

func validateSlice(v interface{}) error {
	if v == nil {
		return nil
	}
	if _, ok := v.([]interface{}); !ok {
		return fmt.Errorf("cannot convert %v (%T) to slice", v, v)
	}
	return nil
}

func scanNullBool(v interface{}) (sql.NullBool, error) {
	if v == nil {
		return sql.NullBool{}, nil
	}
	vv, ok := v.(bool)
	if !ok {
		return sql.NullBool{},
			fmt.Errorf("cannot convert %v (%T) to bool", v, v)
	}
	return sql.NullBool{Valid: true, Bool: vv}, nil
}

// NullSliceBool represents a slice of bool that may be null.
type NullSliceBool struct {
	SliceBool []sql.NullBool
	Valid     bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceBool) Scan(value interface{}) error {
	if value == nil {
		s.SliceBool, s.Valid = []sql.NullBool{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []bool", value, value)
	}
	slice := make([]sql.NullBool, len(vs))
	for i := range vs {
		v, err := scanNullBool(vs[i])
		if err != nil {
			return err
		}
		slice[i] = v
	}
	s.SliceBool = slice
	s.Valid = true
	return nil
}

// NullSlice2Bool represents a two-dimensional slice of bool that may be null.
type NullSlice2Bool struct {
	Slice2Bool [][]sql.NullBool
	Valid      bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2Bool) Scan(value interface{}) error {
	if value == nil {
		s.Slice2Bool, s.Valid = [][]sql.NullBool{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]bool", value, value)
	}
	slice := make([][]sql.NullBool, len(vs))
	for i := range vs {
		var ss NullSliceBool
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceBool
	}
	s.Slice2Bool = slice
	s.Valid = true
	return nil
}

// NullSlice3Bool implements a three-dimensional slice of bool that may be null.
type NullSlice3Bool struct {
	Slice3Bool [][][]sql.NullBool
	Valid      bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3Bool) Scan(value interface{}) error {
	if value == nil {
		s.Slice3Bool, s.Valid = [][][]sql.NullBool{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]bool", value, value)
	}
	slice := make([][][]sql.NullBool, len(vs))
	for i := range vs {
		var ss NullSlice2Bool
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2Bool
	}
	s.Slice3Bool = slice
	s.Valid = true
	return nil
}

func scanNullString(v interface{}) (sql.NullString, error) {
	if v == nil {
		return sql.NullString{}, nil
	}
	vv, ok := v.(string)
	if !ok {
		return sql.NullString{},
			fmt.Errorf("cannot convert %v (%T) to string", v, v)
	}
	return sql.NullString{Valid: true, String: vv}, nil
}

func scanNullBytes(v interface{}) ([]byte, error) {
	if v == nil {
		return nil, nil
	}

	// VARBINARY values come back as a base64 encoded string.
	vv, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("cannot convert %v (%T) to []byte", v, v)
	}

	// Decode the base64 encoded string into a []byte.
	decoded, err := base64.StdEncoding.DecodeString(vv)
	if err != nil {
		return nil, fmt.Errorf("cannot decode base64 string into []byte: %w", err)
	}

	return decoded, nil
}

// NullSliceString represents a slice of string that may be null.
type NullSliceString struct {
	SliceString []sql.NullString
	Valid       bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceString) Scan(value interface{}) error {
	if value == nil {
		s.SliceString, s.Valid = []sql.NullString{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []string", value, value)
	}
	slice := make([]sql.NullString, len(vs))
	for i := range vs {
		v, err := scanNullString(vs[i])
		if err != nil {
			return err
		}
		slice[i] = v
	}
	s.SliceString = slice
	s.Valid = true
	return nil
}

// NullSlice2String represents a two-dimensional slice of string that may be null.
type NullSlice2String struct {
	Slice2String [][]sql.NullString
	Valid        bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2String) Scan(value interface{}) error {
	if value == nil {
		s.Slice2String, s.Valid = [][]sql.NullString{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]string", value, value)
	}
	slice := make([][]sql.NullString, len(vs))
	for i := range vs {
		var ss NullSliceString
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceString
	}
	s.Slice2String = slice
	s.Valid = true
	return nil
}

// NullSlice3String implements a three-dimensional slice of string that may be null.
type NullSlice3String struct {
	Slice3String [][][]sql.NullString
	Valid        bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3String) Scan(value interface{}) error {
	if value == nil {
		s.Slice3String, s.Valid = [][][]sql.NullString{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]string", value, value)
	}
	slice := make([][][]sql.NullString, len(vs))
	for i := range vs {
		var ss NullSlice2String
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2String
	}
	s.Slice3String = slice
	s.Valid = true
	return nil
}

func scanNullInt64(v interface{}) (sql.NullInt64, error) {
	if v == nil {
		return sql.NullInt64{}, nil
	}
	vNumber, ok := v.(json.Number)
	if !ok {
		return sql.NullInt64{},
			fmt.Errorf("cannot convert %v (%T) to int64", v, v)
	}
	vv, err := vNumber.Int64()
	if err != nil {
		return sql.NullInt64{},
			fmt.Errorf("cannot convert %v (%T) to int64", v, v)
	}
	return sql.NullInt64{Valid: true, Int64: vv}, nil
}

// NullSliceInt64 represents a slice of int64 that may be null.
type NullSliceInt64 struct {
	SliceInt64 []sql.NullInt64
	Valid      bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceInt64) Scan(value interface{}) error {
	if value == nil {
		s.SliceInt64, s.Valid = []sql.NullInt64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []int64", value, value)
	}
	slice := make([]sql.NullInt64, len(vs))
	for i := range vs {
		v, err := scanNullInt64(vs[i])
		if err != nil {
			return err
		}
		slice[i] = v
	}
	s.SliceInt64 = slice
	s.Valid = true
	return nil
}

// NullSlice2Int64 represents a two-dimensional slice of int64 that may be null.
type NullSlice2Int64 struct {
	Slice2Int64 [][]sql.NullInt64
	Valid       bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2Int64) Scan(value interface{}) error {
	if value == nil {
		s.Slice2Int64, s.Valid = [][]sql.NullInt64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]int64", value, value)
	}
	slice := make([][]sql.NullInt64, len(vs))
	for i := range vs {
		var ss NullSliceInt64
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceInt64
	}
	s.Slice2Int64 = slice
	s.Valid = true
	return nil
}

// NullSlice3Int64 implements a three-dimensional slice of int64 that may be null.
type NullSlice3Int64 struct {
	Slice3Int64 [][][]sql.NullInt64
	Valid       bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3Int64) Scan(value interface{}) error {
	if value == nil {
		s.Slice3Int64, s.Valid = [][][]sql.NullInt64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]int64", value, value)
	}
	slice := make([][][]sql.NullInt64, len(vs))
	for i := range vs {
		var ss NullSlice2Int64
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2Int64
	}
	s.Slice3Int64 = slice
	s.Valid = true
	return nil
}

func scanNullFloat64(v interface{}) (sql.NullFloat64, error) {
	if v == nil {
		return sql.NullFloat64{}, nil
	}
	vNumber, ok := v.(json.Number)
	if ok {
		vFloat, err := vNumber.Float64()
		if err != nil {
			return sql.NullFloat64{}, fmt.Errorf("cannot convert %v (%T) to float64: %w", vNumber, vNumber, err)
		}
		return sql.NullFloat64{Valid: true, Float64: vFloat}, nil
	}
	switch v {
	case "NaN":
		return sql.NullFloat64{Valid: true, Float64: math.NaN()}, nil
	case "Infinity":
		return sql.NullFloat64{Valid: true, Float64: math.Inf(+1)}, nil
	case "-Infinity":
		return sql.NullFloat64{Valid: true, Float64: math.Inf(-1)}, nil
	default:
		vString, ok := v.(string)
		if !ok {
			return sql.NullFloat64{}, fmt.Errorf("cannot convert %v (%T) to float64", v, v)
		}
		vFloat, err := strconv.ParseFloat(vString, 64)
		if err != nil {
			return sql.NullFloat64{}, fmt.Errorf("cannot convert %v (%T) to float64: %w", v, v, err)
		}
		return sql.NullFloat64{Valid: true, Float64: vFloat}, nil
	}
}

// NullSliceFloat64 represents a slice of float64 that may be null.
type NullSliceFloat64 struct {
	SliceFloat64 []sql.NullFloat64
	Valid        bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceFloat64) Scan(value interface{}) error {
	if value == nil {
		s.SliceFloat64, s.Valid = []sql.NullFloat64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []float64", value, value)
	}
	slice := make([]sql.NullFloat64, len(vs))
	for i := range vs {
		v, err := scanNullFloat64(vs[i])
		if err != nil {
			return err
		}
		slice[i] = v
	}
	s.SliceFloat64 = slice
	s.Valid = true
	return nil
}

// NullSlice2Float64 represents a two-dimensional slice of float64 that may be null.
type NullSlice2Float64 struct {
	Slice2Float64 [][]sql.NullFloat64
	Valid         bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2Float64) Scan(value interface{}) error {
	if value == nil {
		s.Slice2Float64, s.Valid = [][]sql.NullFloat64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]float64", value, value)
	}
	slice := make([][]sql.NullFloat64, len(vs))
	for i := range vs {
		var ss NullSliceFloat64
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceFloat64
	}
	s.Slice2Float64 = slice
	s.Valid = true
	return nil
}

// NullSlice3Float64 represents a three-dimensional slice of float64 that may be null.
type NullSlice3Float64 struct {
	Slice3Float64 [][][]sql.NullFloat64
	Valid         bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3Float64) Scan(value interface{}) error {
	if value == nil {
		s.Slice3Float64, s.Valid = [][][]sql.NullFloat64{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]float64", value, value)
	}
	slice := make([][][]sql.NullFloat64, len(vs))
	for i := range vs {
		var ss NullSlice2Float64
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2Float64
	}
	s.Slice3Float64 = slice
	s.Valid = true
	return nil
}

// Layout for time and timestamp WITHOUT time zone.
// Trino can support up to 12 digits sub second precision, but Go only 9.
// (Requires X-Trino-Client-Capabilities: PARAMETRIC_DATETIME)
var timeLayouts = []string{
	"2006-01-02",
	"15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
}

// Layout for time and timestamp WITH time zone.
// Trino can support up to 12 digits sub second precision, but Go only 9.
// (Requires X-Trino-Client-Capabilities: PARAMETRIC_DATETIME)
var timeLayoutsTZ = []string{
	"15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999 -07:00",
}

func scanNullTime(v interface{}) (NullTime, error) {
	if v == nil {
		return NullTime{}, nil
	}
	vv, ok := v.(string)
	if !ok {
		return NullTime{}, fmt.Errorf("cannot convert %v (%T) to time string", v, v)
	}
	vparts := strings.Split(vv, " ")
	if len(vparts) > 1 && !unicode.IsDigit(rune(vparts[len(vparts)-1][0])) {
		return parseNullTimeWithLocation(vv)
	}
	// Time literals may not have spaces before the timezone.
	if strings.ContainsRune(vv, '+') {
		return parseNullTimeWithLocation(strings.Replace(vv, "+", " +", 1))
	}
	hyphenCount := strings.Count(vv, "-")
	// We need to ensure we don't treat the hyphens in dates as the minus offset sign.
	// So if there's only one hyphen or more than 2, we have a negative offset.
	if hyphenCount == 1 || hyphenCount > 2 {
		// We add a space before the last hyphen to parse properly.
		i := strings.LastIndex(vv, "-")
		timestamp := vv[:i] + strings.Replace(vv[i:], "-", " -", 1)
		return parseNullTimeWithLocation(timestamp)
	}
	return parseNullTime(vv)
}

func parseNullTime(v string) (NullTime, error) {
	var t time.Time
	var err error
	for _, layout := range timeLayouts {
		t, err = time.ParseInLocation(layout, v, time.Local)
		if err == nil {
			return NullTime{Valid: true, Time: t}, nil
		}
	}
	return NullTime{}, err
}

func parseNullTimeWithLocation(v string) (NullTime, error) {
	idx := strings.LastIndex(v, " ")
	if idx == -1 {
		return NullTime{}, fmt.Errorf("cannot convert %v (%T) to time+zone", v, v)
	}
	stamp, location := v[:idx], v[idx+1:]
	var t time.Time
	var err error
	// Try offset timezones.
	if strings.HasPrefix(location, "+") || strings.HasPrefix(location, "-") {
		for _, layout := range timeLayoutsTZ {
			t, err = time.Parse(layout, v)
			if err == nil {
				return NullTime{Valid: true, Time: t}, nil
			}
		}
		return NullTime{}, err
	}
	loc, err := time.LoadLocation(location)
	// Not a named location.
	if err != nil {
		return NullTime{}, fmt.Errorf("cannot load timezone %q: %v", location, err)
	}

	for _, layout := range timeLayouts {
		t, err = time.ParseInLocation(layout, stamp, loc)
		if err == nil {
			return NullTime{Valid: true, Time: t}, nil
		}
	}
	return NullTime{}, err
}

// NullTime represents a time.Time value that can be null.
// The NullTime supports Trino's Date, Time and Timestamp data types,
// with or without time zone.
type NullTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements the sql.Scanner interface.
func (s *NullTime) Scan(value interface{}) error {
	if value == nil {
		s.Time, s.Valid = time.Time{}, false
		return nil
	}
	switch t := value.(type) {
	case time.Time:
		s.Time, s.Valid = t, true
	case NullTime:
		*s = t
	}
	return nil
}

// NullSliceTime represents a slice of time.Time that may be null.
type NullSliceTime struct {
	SliceTime []NullTime
	Valid     bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceTime) Scan(value interface{}) error {
	if value == nil {
		s.SliceTime, s.Valid = []NullTime{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []time.Time", value, value)
	}
	slice := make([]NullTime, len(vs))
	for i := range vs {
		v, err := scanNullTime(vs[i])
		if err != nil {
			return err
		}
		slice[i] = v
	}
	s.SliceTime = slice
	s.Valid = true
	return nil
}

// NullSlice2Time represents a two-dimensional slice of time.Time that may be null.
type NullSlice2Time struct {
	Slice2Time [][]NullTime
	Valid      bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2Time) Scan(value interface{}) error {
	if value == nil {
		s.Slice2Time, s.Valid = [][]NullTime{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]time.Time", value, value)
	}
	slice := make([][]NullTime, len(vs))
	for i := range vs {
		var ss NullSliceTime
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceTime
	}
	s.Slice2Time = slice
	s.Valid = true
	return nil
}

// NullSlice3Time represents a three-dimensional slice of time.Time that may be null.
type NullSlice3Time struct {
	Slice3Time [][][]NullTime
	Valid      bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3Time) Scan(value interface{}) error {
	if value == nil {
		s.Slice3Time, s.Valid = [][][]NullTime{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]time.Time", value, value)
	}
	slice := make([][][]NullTime, len(vs))
	for i := range vs {
		var ss NullSlice2Time
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2Time
	}
	s.Slice3Time = slice
	s.Valid = true
	return nil
}

// NullMap represents a map type that may be null.
type NullMap struct {
	Map   map[string]interface{}
	Valid bool
}

// Scan implements the sql.Scanner interface.
func (m *NullMap) Scan(v interface{}) error {
	if v == nil {
		m.Map, m.Valid = map[string]interface{}{}, false
		return nil
	}
	m.Map, m.Valid = v.(map[string]interface{})
	return nil
}

// NullSliceMap represents a slice of NullMap that may be null.
type NullSliceMap struct {
	SliceMap []NullMap
	Valid    bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSliceMap) Scan(value interface{}) error {
	if value == nil {
		s.SliceMap, s.Valid = []NullMap{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to []NullMap", value, value)
	}
	slice := make([]NullMap, len(vs))
	for i := range vs {
		if err := validateMap(vs[i]); err != nil {
			return fmt.Errorf("cannot convert %v (%T) to []NullMap", value, value)
		}
		m := NullMap{}
		// this scan can never fail
		_ = m.Scan(vs[i])
		slice[i] = m
	}
	s.SliceMap = slice
	s.Valid = true
	return nil
}

// NullSlice2Map represents a two-dimensional slice of NullMap that may be null.
type NullSlice2Map struct {
	Slice2Map [][]NullMap
	Valid     bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice2Map) Scan(value interface{}) error {
	if value == nil {
		s.Slice2Map, s.Valid = [][]NullMap{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][]NullMap", value, value)
	}
	slice := make([][]NullMap, len(vs))
	for i := range vs {
		var ss NullSliceMap
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.SliceMap
	}
	s.Slice2Map = slice
	s.Valid = true
	return nil
}

// NullSlice3Map represents a three-dimensional slice of NullMap that may be null.
type NullSlice3Map struct {
	Slice3Map [][][]NullMap
	Valid     bool
}

// Scan implements the sql.Scanner interface.
func (s *NullSlice3Map) Scan(value interface{}) error {
	if value == nil {
		s.Slice3Map, s.Valid = [][][]NullMap{}, false
		return nil
	}
	vs, ok := value.([]interface{})
	if !ok {
		return fmt.Errorf("trino: cannot convert %v (%T) to [][][]NullMap", value, value)
	}
	slice := make([][][]NullMap, len(vs))
	for i := range vs {
		var ss NullSlice2Map
		if err := ss.Scan(vs[i]); err != nil {
			return err
		}
		slice[i] = ss.Slice2Map
	}
	s.Slice3Map = slice
	s.Valid = true
	return nil
}

type QueryProgressInfo struct {
	QueryId    string
	QueryStats stmtStats
}

type queryProgressCallbackPeriod struct {
	Period           time.Duration
	LastCallbackTime time.Time
	LastQueryState   string
}

type ProgressUpdater interface {
	// Update the query progress, immediately when the query starts, when receiving data, and once when the query is finished.
	Update(QueryProgressInfo)
}
