// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf

package usm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/net/http2/hpack"

	"github.com/DataDog/datadog-agent/pkg/ebpf/ebpftest"
	"github.com/DataDog/datadog-agent/pkg/network/config"
	"github.com/DataDog/datadog-agent/pkg/network/protocols"
	usmhttp "github.com/DataDog/datadog-agent/pkg/network/protocols/http"
	"github.com/DataDog/datadog-agent/pkg/network/protocols/http/testutil"
	usmhttp2 "github.com/DataDog/datadog-agent/pkg/network/protocols/http2"
	gotlsutils "github.com/DataDog/datadog-agent/pkg/network/protocols/tls/gotls/testutil"
	"github.com/DataDog/datadog-agent/pkg/network/tracer/testutil/proxy"
	"github.com/DataDog/datadog-agent/pkg/network/usm/utils"
	"github.com/DataDog/datadog-agent/pkg/util/kernel"
)

const (
	srvPort               = 8082
	unixPath              = "/tmp/transparent.sock"
	pathWithStatusCode300 = "/raw-test-300"
	pathWithStatusCode401 = "/raw-test-401"
	http2DefaultTestPath  = "/aaa"
)

var (
	authority    = net.JoinHostPort("127.0.0.1", strconv.Itoa(srvPort))
	http2SrvAddr = "http://" + authority
)

type usmHTTP2Suite struct {
	suite.Suite
	isTLS bool
}

func (s *usmHTTP2Suite) getCfg() *config.Config {
	cfg := config.New()
	cfg.EnableHTTP2Monitoring = true
	cfg.EnableGoTLSSupport = s.isTLS
	cfg.GoTLSExcludeSelf = s.isTLS
	return cfg
}

func TestHTTP2Scenarios(t *testing.T) {
	currKernelVersion, err := kernel.HostVersion()
	require.NoError(t, err)
	if currKernelVersion < usmhttp2.MinimumKernelVersion {
		t.Skipf("HTTP2 monitoring can not run on kernel before %v", usmhttp2.MinimumKernelVersion)
	}

	ebpftest.TestBuildModes(t, []ebpftest.BuildMode{ebpftest.Prebuilt, ebpftest.RuntimeCompiled, ebpftest.CORE}, "", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			isTLS bool
		}{
			{
				name:  "without TLS",
				isTLS: false,
			},
			{
				name:  "with TLS",
				isTLS: true,
			},
		} {
			if tc.isTLS && !gotlsutils.GoTLSSupported(t, config.New()) {
				t.Skip("GoTLS not supported for this setup")
			}
			t.Run(tc.name, func(t *testing.T) {
				suite.Run(t, &usmHTTP2Suite{isTLS: tc.isTLS})
			})
		}
	})
}

func (s *usmHTTP2Suite) TestHTTP2DynamicTableCleanup() {
	t := s.T()
	cfg := s.getCfg()
	cfg.HTTP2DynamicTableMapCleanerInterval = 5 * time.Second

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	monitor := setupUSMTLSMonitor(t, cfg)
	if s.isTLS {
		utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
	}

	clients := getHTTP2UnixClientArray(2, unixPath)
	for i := 0; i < usmhttp2.HTTP2TerminatedBatchSize; i++ {
		req, err := clients[i%2].Post(fmt.Sprintf("%s/test-%d", http2SrvAddr, i+1), "application/json", bytes.NewReader([]byte("test")))
		require.NoError(t, err, "could not make request")
		req.Body.Close()
	}

	matches := PrintableInt(0)

	require.Eventuallyf(t, func() bool {
		for key, stat := range getHTTPLikeProtocolStats(monitor, protocols.HTTP2) {
			if (key.DstPort == srvPort || key.SrcPort == srvPort) && key.Method == usmhttp.MethodPost && strings.HasPrefix(key.Path.Content.Get(), "/test") {
				matches.Add(stat.Data[200].Count)
			}
		}

		return matches.Load() == usmhttp2.HTTP2TerminatedBatchSize
	}, time.Second*10, time.Millisecond*100, "%v != %v", &matches, usmhttp2.HTTP2TerminatedBatchSize)

	for _, client := range clients {
		client.CloseIdleConnections()
	}

	dynamicTableMap, _, err := monitor.ebpfProgram.GetMap("http2_dynamic_table")
	require.NoError(t, err)
	iterator := dynamicTableMap.Iterate()
	key := make([]byte, dynamicTableMap.KeySize())
	value := make([]byte, dynamicTableMap.ValueSize())
	count := 0
	for iterator.Next(&key, &value) {
		count++
	}
	require.GreaterOrEqual(t, count, 0)

	require.Eventually(t, func() bool {
		iterator = dynamicTableMap.Iterate()
		count = 0
		for iterator.Next(&key, &value) {
			count++
		}

		return count == 0
	}, cfg.HTTP2DynamicTableMapCleanerInterval*4, time.Millisecond*100)
}

func (s *usmHTTP2Suite) TestSimpleHTTP2() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	tests := []struct {
		name              string
		runClients        func(t *testing.T, clientsCount int)
		expectedEndpoints map[usmhttp.Key]captureRange
	}{
		{
			name: " / path",
			runClients: func(t *testing.T, clientsCount int) {
				clients := getHTTP2UnixClientArray(clientsCount, unixPath)

				for i := 0; i < 1000; i++ {
					client := clients[getClientsIndex(i, clientsCount)]
					req, err := client.Post(http2SrvAddr+"/", "application/json", bytes.NewReader([]byte("test")))
					require.NoError(t, err, "could not make request")
					req.Body.Close()
				}
			},
			expectedEndpoints: map[usmhttp.Key]captureRange{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString("/")},
					Method: usmhttp.MethodPost,
				}: {
					lower: 999,
					upper: 1001,
				},
			},
		},
		{
			name: " /index.html path",
			runClients: func(t *testing.T, clientsCount int) {
				clients := getHTTP2UnixClientArray(clientsCount, unixPath)

				for i := 0; i < 1000; i++ {
					client := clients[getClientsIndex(i, clientsCount)]
					req, err := client.Post(http2SrvAddr+"/index.html", "application/json", bytes.NewReader([]byte("test")))
					require.NoError(t, err, "could not make request")
					req.Body.Close()
				}
			},
			expectedEndpoints: map[usmhttp.Key]captureRange{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString("/index.html")},
					Method: usmhttp.MethodPost,
				}: {
					lower: 999,
					upper: 1001,
				},
			},
		},
		{
			name: "path with repeated string",
			runClients: func(t *testing.T, clientsCount int) {
				clients := getHTTP2UnixClientArray(clientsCount, unixPath)

				for i := 1; i < 100; i++ {
					path := strings.Repeat("a", i)
					client := clients[getClientsIndex(i, clientsCount)]
					req, err := client.Post(http2SrvAddr+"/"+path, "application/json", bytes.NewReader([]byte("test")))
					require.NoError(t, err, "could not make request")
					req.Body.Close()
				}
			},
			expectedEndpoints: getExpectedOutcomeForPathWithRepeatedChars(),
		},
	}
	for _, tt := range tests {
		for _, clientCount := range []int{1, 2, 5} {
			testNameSuffix := fmt.Sprintf("-different clients - %v", clientCount)
			t.Run(tt.name+testNameSuffix, func(t *testing.T) {
				monitor := setupUSMTLSMonitor(t, cfg)
				if s.isTLS {
					utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
				}
				tt.runClients(t, clientCount)

				res := make(map[usmhttp.Key]int)
				assert.Eventually(t, func() bool {
					for key, stat := range getHTTPLikeProtocolStats(monitor, protocols.HTTP2) {
						if key.DstPort == srvPort || key.SrcPort == srvPort {
							count := stat.Data[200].Count
							newKey := usmhttp.Key{
								Path:   usmhttp.Path{Content: key.Path.Content},
								Method: key.Method,
							}
							if _, ok := res[newKey]; !ok {
								res[newKey] = count
							} else {
								res[newKey] += count
							}
						}
					}

					if len(res) != len(tt.expectedEndpoints) {
						return false
					}

					for key, count := range res {
						valRange, ok := tt.expectedEndpoints[key]
						if !ok {
							return false
						}
						if count < valRange.lower || count > valRange.upper {
							return false
						}
					}

					return true
				}, time.Second*5, time.Millisecond*100, "%v != %v", res, tt.expectedEndpoints)
				if t.Failed() {
					for key := range tt.expectedEndpoints {
						if _, ok := res[key]; !ok {
							t.Logf("key: %v was not found in res", key.Path.Content.Get())
						}
					}
					ebpftest.DumpMapsTestHelper(t, monitor.DumpMaps, "http2_in_flight")
				}
			})
		}
	}
}

var (
	// pathExceedingMaxSize is path with size 166, which is exceeding the maximum path size in the kernel (HTTP2_MAX_PATH_LEN).
	pathExceedingMaxSize = "X2YRUwfeNEmYWkk0bACThVya8MoSUkR7ZKANCPYkIGHvF9CWGA0rxXKsGogQag7HsJfmgaar3TiOTRUb3ynbmiOz3As9rXYjRGNRdCWGgdBPL8nGa6WheGlJLNtIVsUcxSerNQKmoQqqDLjGftbKXjqdMJLVY6UyECeXOKrrFU9aHx2fjlk2qMNDUptYWuzPPCWAnKOV7Ph"

	http2UniquePaths = []string{
		// size 82 bucket 0
		"C9ZaSMOpthT9XaRh9yc6AKqfIjT43M8gOz3p9ASKCNRIcLbc3PTqEoms2SDwt6Q90QM7DxjWKlmZUfRU1eOx5DjQOOhLaIJQke4N",
		// size 127 bucket 1
		"ZtZuUQeVB7BOl3F45oFicOOOJl21ePFwunMBvBh3bXPMBZqdEZepVsemYA0frZb5M83VHLDWq68KFELDHu0Xo28lzpzO3L7kDXuYuClivgEgURUn47kfwfUfW1PKjfsV6HaYpAZxly48lTGiRIXRINVC8b9",
		// size 137, bucket 2
		"RDBVk5COXAz52GzvuHVWRawNoKhmfxhBiTuyj5QZ6qR1DMsNOn4sWFLnaGXVzrqA8NLr2CaW1IDupzh9AzJlIvgYSf6OYIafIOsImL5O9M3AHzUHGMJ0KhjYGJAzXeTgvwl2qYWmlD9UYGELFpBSzJpykoriocvl3RRoYt4l",
		// size 147, bucket 3
		"T5r8QcP8qCiKVwhWlaxWjYCX8IrTmPrt2HRjfQJP2PxbWjLm8dP4BTDxUAmXJJWNyv4HIIaR3Fj6n8Tu6vSoDcBtKFuMqIPAdYEJt0qo2aaYDKomIJv74z7SiN96GrOufPTm6Eutl3JGeAKW2b0dZ4VYUsIOO8aheEOGmyhyWBymgCtBcXeki1",
		// size 158, bucket 4
		"VP4zOrIPiGhLDLSJYSVU78yUcb8CkU0dVDIZqPq98gVoenX5p1zS6cRX4LtrfSYKCQFX6MquluhDD2GPjZYFIraDLIHCno3yipQBLPGcPbPTgv9SD6jOlHMuLjmsGxyC3y2Hk61bWA6Af4D2SYS0q3BS7ahJ0vjddYYBRIpwMOOIez2jaR56rPcGCRW2eq0T1x",
		// size 166, bucket 5
		pathExceedingMaxSize,
		// size 172, bucket 6
		"bq5bcpUgiW1CpKgwdRVIulFMkwRenJWYdW8aek69anIV8w3br0pjGNtfnoPCyj4HUMD5MxWB2xM4XGp7fZ1JRHvskRZEgmoM7ag9BeuigmH05p7dzMwKsD76MqKyPmfhwBUZHLKtJ52ia3mOuMvyYiQNwA6KAU509bwuy4NCREVUAP76WFeAzr0jBvqMFXLg3eQQERIW0tKTcjQg8m9Jse",
		// size 247, bucket 7
		"LUhWUWPMztVFuEs83i7RmoxRiV1KzOq0NsZmGXVyW49BbBaL63m8H5vDwiewrrKbldXBuctplDxB28QekDclM6cO9BIsRqvzS3a802aOkRHTEruotA8Xh5K9GOMv9DzdoOL9P3GFPsUPgBy0mzFyyRJGk3JXpIH290Bj2FIRnIIpIjjKE1akeaimsuGEheA4D95axRpGmz4cm2s74UiksfBi4JnVX2cBzZN3oQaMt7zrWofwyzcZeF5W1n6BAQWxPPWe4Jyoc34jQ2fiEXQO0NnXe1RFbBD1E33a0OycziXZH9hEP23xvh",
	}
)

func (s *usmHTTP2Suite) TestHTTP2KernelTelemetry() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	tests := []struct {
		name              string
		runClients        func(t *testing.T, clientsCount int)
		expectedTelemetry *usmhttp2.HTTP2Telemetry
	}{
		{
			name: "Fill each bucket",
			runClients: func(t *testing.T, clientsCount int) {
				clients := getHTTP2UnixClientArray(clientsCount, unixPath)
				for _, path := range http2UniquePaths {
					client := clients[getClientsIndex(1, clientsCount)]
					req, err := client.Post(http2SrvAddr+"/"+path, "application/json", bytes.NewReader([]byte("test")))
					require.NoError(t, err, "could not make request")
					req.Body.Close()
				}
			},

			expectedTelemetry: &usmhttp2.HTTP2Telemetry{
				Request_seen:      8,
				Response_seen:     8,
				End_of_stream:     16,
				End_of_stream_rst: 0,
				Path_size_bucket:  [8]uint64{1, 1, 1, 1, 1, 1, 1, 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupUSMTLSMonitor(t, cfg)
			if s.isTLS {
				utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
			}

			tt.runClients(t, 1)

			// We cannot predict if the client will send an RST frame or not, thus we cannot predict the number of
			// frames with EOS or RST frames, which leads into a flaking test. Therefore, we are asserting that the
			// gotten number of EOS or RST frames is at least the number of expected EOS frames.
			expectedEOSOrRST := tt.expectedTelemetry.End_of_stream + tt.expectedTelemetry.End_of_stream_rst
			var telemetry *usmhttp2.HTTP2Telemetry
			assert.Eventually(t, func() bool {
				telemetry, err = usmhttp2.Spec.Instance.(*usmhttp2.Protocol).GetHTTP2KernelTelemetry()
				require.NoError(t, err)
				if telemetry.Request_seen != tt.expectedTelemetry.Request_seen {
					return false
				}
				if telemetry.Response_seen != tt.expectedTelemetry.Response_seen {
					return false
				}
				if telemetry.Path_exceeds_frame != tt.expectedTelemetry.Path_exceeds_frame {
					return false
				}
				if telemetry.Exceeding_max_interesting_frames != tt.expectedTelemetry.Exceeding_max_interesting_frames {
					return false
				}
				if telemetry.Exceeding_max_frames_to_filter != tt.expectedTelemetry.Exceeding_max_frames_to_filter {
					return false
				}
				if telemetry.End_of_stream+telemetry.End_of_stream_rst < expectedEOSOrRST {
					return false
				}
				return reflect.DeepEqual(telemetry.Path_size_bucket, tt.expectedTelemetry.Path_size_bucket)

			}, time.Second*5, time.Millisecond*100)
			if t.Failed() {
				t.Logf("expected telemetry: %+v;\ngot: %+v", tt.expectedTelemetry, telemetry)
			}
		})
	}
}

func (s *usmHTTP2Suite) TestHTTP2ManyDifferentPaths() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	monitor := setupUSMTLSMonitor(t, cfg)
	if s.isTLS {
		utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
	}

	const (
		repetitionsPerRequest = 2
		// Should be bigger than the length of the http2_dynamic_table which is 1024
		numberOfRequests         = 1500
		expectedNumberOfRequests = numberOfRequests * repetitionsPerRequest
	)
	clients := getHTTP2UnixClientArray(1, unixPath)
	for i := 0; i < numberOfRequests; i++ {
		for j := 0; j < repetitionsPerRequest; j++ {
			req, err := clients[0].Post(fmt.Sprintf("%s/test-%d", http2SrvAddr, i+1), "application/json", bytes.NewReader([]byte("test")))
			require.NoError(t, err, "could not make request")
			req.Body.Close()
		}
	}

	matches := PrintableInt(0)

	seenRequests := map[string]int{}
	assert.Eventuallyf(t, func() bool {
		for key, stat := range getHTTPLikeProtocolStats(monitor, protocols.HTTP2) {
			if (key.DstPort == srvPort || key.SrcPort == srvPort) && key.Method == usmhttp.MethodPost && strings.HasPrefix(key.Path.Content.Get(), "/test") {
				if _, ok := seenRequests[key.Path.Content.Get()]; !ok {
					seenRequests[key.Path.Content.Get()] = 0
				}
				seenRequests[key.Path.Content.Get()] += stat.Data[200].Count
				matches.Add(stat.Data[200].Count)
			}
		}

		// Due to a known issue in http2, we might consider an RST packet as a response to a request and therefore
		// we might capture a request twice. This is why we are expecting to see 2*numberOfRequests instead of
		return (expectedNumberOfRequests-1) <= matches.Load() && matches.Load() <= (expectedNumberOfRequests+1)
	}, time.Second*10, time.Millisecond*100, "%v != %v", &matches, expectedNumberOfRequests)

	for i := 0; i < numberOfRequests; i++ {
		if v, ok := seenRequests[fmt.Sprintf("/test-%d", i+1)]; !ok || v != repetitionsPerRequest {
			t.Logf("path: /test-%d should have %d occurrences but instead has %d", i+1, repetitionsPerRequest, v)
		}
	}
}

func (s *usmHTTP2Suite) TestRawTraffic() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	getTLSNumber := func(numberWithoutTLS, numberWithTLS int, isTLS bool) int {
		if isTLS {
			return numberWithTLS
		}
		return numberWithoutTLS
	}

	tests := []struct {
		name              string
		skip              bool
		messageBuilder    func() []byte
		expectedEndpoints map[usmhttp.Key]int
	}{
		{
			name: "parse_frames tail call using 1 program",
			// The objective of this test is to verify that we accurately perform the parsing of frames within
			// a single program.
			messageBuilder: func() []byte {
				const settingsFramesCount = 100
				framer := newFramer()
				return framer.
					writeMultiMessage(t, settingsFramesCount, framer.writeSettings).
					writeHeaders(t, getStreamID(1), testHeaders()).
					writeData(t, getStreamID(1), true, []byte{}).
					bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 1,
			},
		},
		{
			name: "parse_frames tail call using 2 programs",
			// The purpose of this test is to validate that when we surpass the limit of HTTP2_MAX_FRAMES_ITERATIONS,
			// the filtering of subsequent frames will continue using tail calls.
			messageBuilder: func() []byte {
				const settingsFramesCount = 130
				framer := newFramer()
				return framer.
					writeMultiMessage(t, settingsFramesCount, framer.writeSettings).
					writeHeaders(t, getStreamID(1), testHeaders()).
					writeData(t, getStreamID(1), true, []byte{}).
					bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 1,
			},
			// Currently we don't have a way to test it as TLS version does not have the ability to run 2 tail calls
			// for filtering frames (will be fixed in USMON-684)
			skip: s.isTLS,
		},
		{
			name: "validate frames_filter tail calls limit",
			// The purpose of this test is to validate that when we surpass the limit of HTTP2_MAX_TAIL_CALLS_FOR_FRAMES_FILTER,
			// for 2 filter_frames we do not use more than two tail calls.
			messageBuilder: func() []byte {
				settingsFramesCount := getTLSNumber(241, 121, s.isTLS)
				framer := newFramer()
				return framer.
					writeMultiMessage(t, settingsFramesCount, framer.writeSettings).
					writeHeaders(t, getStreamID(1), testHeaders()).
					writeData(t, getStreamID(1), true, []byte{}).
					bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate max interesting frames limit",
			// The purpose of this test is to verify our ability to reach the limit set by HTTP2_MAX_FRAMES_ITERATIONS, which
			// determines the maximum number of "interesting frames" we can process.
			messageBuilder: func() []byte {
				iterations := getTLSNumber(120, 60, s.isTLS)
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, testHeaders()).
						writeData(t, streamID, true, []byte{})
				}
				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: getTLSNumber(120, 60, s.isTLS),
			},
		},
		{
			name: "validate literal header field without indexing",
			// The purpose of this test is to verify our ability the case:
			// Literal Header Field without Indexing (0b0000xxxx: top four bits are 0000)
			// https://httpwg.org/specs/rfc7541.html#rfc.section.C.2.2
			messageBuilder: func() []byte {
				const iterations = 5
				const setDynamicTableSize = true
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, headersWithoutIndexingPath(), setDynamicTableSize).
						writeData(t, streamID, true, []byte{})
				}
				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString("/" + strings.Repeat("a", usmhttp2.DynamicTableSize))},
					Method: usmhttp.MethodPost,
				}: 5,
			},
		},
		{
			name: "validate literal header field never indexed",
			// The purpose of this test is to verify our ability the case:
			// Literal Header Field never Indexed (0b0001xxxx: top four bits are 0001)
			// https://httpwg.org/specs/rfc7541.html#rfc.section.6.2.3
			messageBuilder: func() []byte {
				const iterations = 5
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, headersWithNeverIndexedPath()).
						writeData(t, streamID, true, []byte{})
				}
				return framer.bytes()

			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 5,
			},
		},
		{
			name: "validate path with index 4",
			// The purpose of this test is to verify our ability to identify paths with index 4.
			messageBuilder: func() []byte {
				const iterations = 5
				// pathHeaderField is the hex representation of the path /aaa with index 4.
				pathHeaderField := []byte{0x44, 0x83, 0x60, 0x63, 0x1f}
				headerFields := removeHeaderFieldByKey(testHeaders(), ":path")
				headersFrame, err := usmhttp2.NewHeadersFrameMessage(headerFields)
				require.NoError(t, err, "could not create headers frame")

				// we are adding the path header field with index 4, we need to do it on the byte slice and not on the headerFields
				// due to the fact that when we create a header field it would be with index 5.
				headersFrame = append(pathHeaderField, headersFrame...)
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeRawHeaders(t, streamID, headersFrame).
						writeData(t, streamID, true, []byte{})
				}
				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 5,
			},
		},
		{
			name: "validate PING and WINDOWS_UPDATE frames between HEADERS and DATA",
			// The purpose of this test is to verify our ability to process PING and WINDOWS_UPDATE frames between HEADERS and DATA.
			messageBuilder: func() []byte {
				const iterations = 5
				framer := newFramer()

				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, testHeaders()).
						writePing(t).
						writeWindowUpdate(t, streamID, 1).
						writeData(t, streamID, true, []byte{})
				}

				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 5,
			},
		},
		{
			name: "validate RST_STREAM cancel err code",
			// The purpose of this test is to validate that when a cancel error code is sent, we will not count the request.
			// We are sending 10 requests, and 5 of them will contain RST_STREAM with a cancel error code.Therefore, we expect to
			// capture five valid requests.
			messageBuilder: func() []byte {
				const iterations = 10
				rstFramesCount := 5
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, testHeaders(), false).
						writeData(t, streamID, true, []byte{})
					if rstFramesCount > 0 {
						framer.writeRSTStream(t, streamID, http2.ErrCodeCancel)
						rstFramesCount--
					}
				}
				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 5,
			},
		},
		{
			name: "validate RST_STREAM before server status ok",
			// The purpose of this test is to validate that when we see RST before DATA frame with status ok,
			// we will not count the requests.
			messageBuilder: func() []byte {
				const iterations = 10
				framer := newFramer()
				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, testHeaders(), false).
						writeData(t, streamID, true, []byte{}).
						writeRSTStream(t, streamID, http2.ErrCodeNo)
				}
				return framer.bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate capability to process up to max limit filtering frames",
			// The purpose of this test is to verify our ability to process up to HTTP2_MAX_HEADERS_COUNT_FOR_FILTERING frames.
			// We write the path "/aaa" for the first time with an additional 25 headers (reaching to a total of 26 headers).
			// When we exceed the limit, we expect to lose our internal counter (because we can filter up to 25 requests),
			// and therefore, the next time we write the request "/aaa",
			// its internal index will not be correct, and we will not be able to find it.
			messageBuilder: func() []byte {
				const multiHeadersCount = 25
				framer := newFramer()
				return framer.writeHeaders(t, 1, multipuleTestHeaders(multiHeadersCount)).
					writeData(t, 1, true, []byte{}).
					writeHeaders(t, 1, testHeaders()).
					writeData(t, 1, true, []byte{}).bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate 300 status code",
			// The purpose of this test is to verify that currently we do not support status code 300.
			messageBuilder: func() []byte {
				framer := newFramer()
				return framer.writeHeaders(t, 1, headersWithGivenEndpoint(pathWithStatusCode300)).
					writeData(t, 1, true, []byte{}).bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate 401 status code",
			// The purpose of this test is to verify that currently we do not support status code 401.
			messageBuilder: func() []byte {
				framer := newFramer()
				return framer.writeHeaders(t, 1, headersWithGivenEndpoint(pathWithStatusCode401)).
					writeData(t, 1, true, []byte{}).bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate http methods",
			// The purpose of this test is to validate that we are not supporting http2 methods different from POST and GET.
			messageBuilder: func() []byte {
				httpMethods = []string{http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions}
				framer := newFramer()
				for i, method := range httpMethods {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, headersWithGivenPathMethod(method), false).
						writeData(t, streamID, true, []byte{})
				}
				return framer.bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate max path length",
			// The purpose of this test is to validate that we are not able to process a path longer than HTTP2_MAX_PATH_LEN.
			messageBuilder: func() []byte {
				framer := newFramer()
				return framer.
					writeHeaders(t, getStreamID(1), headersWithExceedingPathLen(), false).
					writeData(t, getStreamID(1), true, []byte{}).bytes()
			},
			expectedEndpoints: nil,
		},
		{
			name: "validate path sent by value (:path)",
			// The purpose of this test is to verify our ability to identify paths which were sent with a key that
			// sent by value (:path).
			messageBuilder: func() []byte {
				headerFields := removeHeaderFieldByKey(testHeaders(), ":path")
				headersFrame, err := usmhttp2.NewHeadersFrameMessage(headerFields)
				require.NoError(t, err, "could not create headers frame")
				// pathHeaderField is created with a key that sent by value (:path) and
				// the value (of the path) is /aaa.
				pathHeaderField := []byte{0x40, 0x84, 0xb9, 0x58, 0xd3, 0x3f, 0x83, 0x60, 0x63, 0x1f}
				headersFrame = append(pathHeaderField, headersFrame...)
				framer := newFramer()
				return framer.writeRawHeaders(t, 1, headersFrame).
					writeData(t, 1, true, []byte{}).bytes()
			},
			expectedEndpoints: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip {
				t.Skip("skipping test")
			}

			usmMonitor := setupUSMTLSMonitor(t, cfg)
			if s.isTLS {
				utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
			}

			c, err := net.Dial("unix", unixPath)
			require.NoError(t, err, "could not dial")
			defer c.Close()

			// Create a buffer to write the frame into.
			var buf bytes.Buffer
			framer := http2.NewFramer(&buf, nil)
			// Write the empty SettingsFrame to the buffer using the Framer
			require.NoError(t, framer.WriteSettings(http2.Setting{}), "could not write settings frame")

			// Writing a magic and the settings in the same packet to socket.
			require.NoError(t, writeInput(c, usmhttp2.ComposeMessage([]byte(http2.ClientPreface), buf.Bytes()), time.Second))

			// Composing a message with the number of setting frames we want to send.
			require.NoError(t, writeInput(c, tt.messageBuilder(), time.Second))

			res := make(map[usmhttp.Key]int)
			assert.Eventually(t, func() bool {
				return validateStats(usmMonitor, res, tt.expectedEndpoints)
			}, time.Second*5, time.Millisecond*100, "%v != %v", res, tt.expectedEndpoints)
			if t.Failed() {
				for key := range tt.expectedEndpoints {
					if _, ok := res[key]; !ok {
						t.Logf("key: %v was not found in res", key.Path.Content.Get())
					}
				}
				ebpftest.DumpMapsTestHelper(t, usmMonitor.DumpMaps, "http2_in_flight")
			}
		})
	}
}

func (s *usmHTTP2Suite) TestDynamicTable() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	tests := []struct {
		name                            string
		skip                            bool
		messageBuilder                  func() []byte
		expectedEndpoints               map[usmhttp.Key]int
		expectedDynamicTablePathIndexes []int
	}{
		{
			name: "dynamic table contains only index of path",
			// The purpose of this test is to validate that the dynamic table contains only paths indexes.
			messageBuilder: func() []byte {
				const iterations = 10
				framer := newFramer()

				for i := 0; i < iterations; i++ {
					streamID := getStreamID(i)
					framer.
						writeHeaders(t, streamID, testHeaders()).
						writeData(t, streamID, true, []byte{})
				}

				return framer.bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 10,
			},
			expectedDynamicTablePathIndexes: []int{1, 7, 13, 19, 25, 31, 37, 43, 49, 55},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			usmMonitor := setupUSMTLSMonitor(t, cfg)
			if s.isTLS {
				utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
			}

			c, err := net.Dial("unix", unixPath)
			require.NoError(t, err, "could not dial")
			defer c.Close()

			// Create a buffer to write the frame into.
			var buf bytes.Buffer
			framer := http2.NewFramer(&buf, nil)
			// Write the empty SettingsFrame to the buffer using the Framer
			require.NoError(t, framer.WriteSettings(http2.Setting{}), "could not write settings frame")

			// Writing a magic and the settings in the same packet to socket.
			require.NoError(t, writeInput(c, usmhttp2.ComposeMessage([]byte(http2.ClientPreface), buf.Bytes()), time.Second))

			// Composing a message with the number of setting frames we want to send.
			require.NoError(t, writeInput(c, tt.messageBuilder(), time.Second))

			res := make(map[usmhttp.Key]int)
			assert.Eventually(t, func() bool {
				// validate the stats we get
				require.True(t, validateStats(usmMonitor, res, tt.expectedEndpoints))

				validateDynamicTableMap(t, usmMonitor.ebpfProgram, tt.expectedDynamicTablePathIndexes)

				return true
			}, time.Second*5, time.Millisecond*100, "%v != %v", res, tt.expectedEndpoints)
			if t.Failed() {
				for key := range tt.expectedEndpoints {
					if _, ok := res[key]; !ok {
						t.Logf("key: %v was not found in res", key.Path.Content.Get())
					}
				}
				ebpftest.DumpMapsTestHelper(t, usmMonitor.DumpMaps, "http2_in_flight")
			}
		})
	}
}

func (s *usmHTTP2Suite) TestRawHuffmanEncoding() {
	t := s.T()
	cfg := s.getCfg()

	// Start local server
	cleanup, err := startH2CServer(authority, s.isTLS)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Start the proxy server.
	proxyProcess, cancel := proxy.NewExternalUnixTransparentProxyServer(t, unixPath, authority, s.isTLS)
	t.Cleanup(cancel)
	require.NoError(t, proxy.WaitForConnectionReady(unixPath))

	tests := []struct {
		name                   string
		skip                   bool
		messageBuilder         func() []byte
		expectedEndpoints      map[usmhttp.Key]int
		expectedHuffmanEncoded map[int]bool
	}{
		{
			name: "validate huffman encoding",
			// The purpose of this test is to verify that we are able to identify if the path is huffman encoded.
			messageBuilder: func() []byte {
				framer := newFramer()
				return framer.writeHeaders(t, 1, testHeaders()).
					writeData(t, 1, true, []byte{}).
					writeHeaders(t, 3, headersWithGivenEndpoint("/a")).
					writeData(t, 3, true, []byte{}).bytes()
			},
			expectedEndpoints: map[usmhttp.Key]int{
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString(http2DefaultTestPath)},
					Method: usmhttp.MethodPost,
				}: 1,
				{
					Path:   usmhttp.Path{Content: usmhttp.Interner.GetString("/a")},
					Method: usmhttp.MethodPost,
				}: 1,
			},
			// the key is the path size, and the value is if it should be huffman encoded or not.
			expectedHuffmanEncoded: map[int]bool{
				2: false,
				3: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usmMonitor := setupUSMTLSMonitor(t, cfg)
			if s.isTLS {
				utils.WaitForProgramsToBeTraced(t, "go-tls", proxyProcess.Process.Pid)
			}

			c, err := net.Dial("unix", unixPath)
			require.NoError(t, err, "could not dial")
			defer c.Close()

			// Create a buffer to write the frame into.
			var buf bytes.Buffer
			framer := http2.NewFramer(&buf, nil)
			// Write the empty SettingsFrame to the buffer using the Framer
			require.NoError(t, framer.WriteSettings(http2.Setting{}), "could not write settings frame")

			// Writing a magic and the settings in the same packet to socket.
			require.NoError(t, writeInput(c, usmhttp2.ComposeMessage([]byte(http2.ClientPreface), buf.Bytes()), time.Second))

			// Composing a message with the number of setting frames we want to send.
			require.NoError(t, writeInput(c, tt.messageBuilder(), time.Second))

			res := make(map[usmhttp.Key]int)
			assert.Eventually(t, func() bool {
				// validate the stats we get
				require.True(t, validateStats(usmMonitor, res, tt.expectedEndpoints))

				validateHuffmanEncoded(t, usmMonitor.ebpfProgram, tt.expectedHuffmanEncoded)

				return true
			}, time.Second*5, time.Millisecond*100, "%v != %v", res, tt.expectedEndpoints)
			if t.Failed() {
				for key := range tt.expectedEndpoints {
					if _, ok := res[key]; !ok {
						t.Logf("key: %v was not found in res", key.Path.Content.Get())
					}
				}
				ebpftest.DumpMapsTestHelper(t, usmMonitor.DumpMaps, "http2_in_flight")
			}
		})
	}
}

// validateStats validates that the stats we get from the monitor are as expected.
func validateStats(usmMonitor *Monitor, res map[usmhttp.Key]int, expectedEndpoints map[usmhttp.Key]int) bool {
	for key, stat := range getHTTPLikeProtocolStats(usmMonitor, protocols.HTTP2) {
		if key.DstPort == srvPort || key.SrcPort == srvPort {
			count := stat.Data[200].Count
			newKey := usmhttp.Key{
				Path:   usmhttp.Path{Content: key.Path.Content},
				Method: key.Method,
			}
			if _, ok := res[newKey]; !ok {
				res[newKey] = count
			} else {
				res[newKey] += count
			}
		}
	}

	if len(res) != len(expectedEndpoints) {
		return false
	}

	for key, endpointCount := range res {
		_, ok := expectedEndpoints[key]
		if !ok {
			return false
		}
		if endpointCount != expectedEndpoints[key] {
			return false
		}
	}
	return true
}

// getHTTP2UnixClientArray creates an array of http2 clients over a unix socket.
func getHTTP2UnixClientArray(size int, unixPath string) []*http.Client {
	res := make([]*http.Client, size)
	for i := 0; i < size; i++ {
		res[i] = &http.Client{
			Transport: &http2.Transport{
				AllowHTTP: true,
				DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
					return net.Dial("unix", unixPath)
				},
			},
		}
	}

	return res
}

// writeInput writes the given input to the socket and reads the response.
// Presently, the timeout is configured to one second for all readings.
// In case of encountered issues, increasing this duration might be necessary.
func writeInput(c net.Conn, input []byte, timeout time.Duration) error {
	_, err := c.Write(input)
	if err != nil {
		return err
	}
	frame := make([]byte, 9)
	// Since we don't know when to stop reading from the socket, we set a timeout.
	c.SetReadDeadline(time.Now().Add(timeout))
	for {
		// Read the frame header.
		_, err := c.Read(frame)
		if err != nil {
			// we want to stop reading from the socket when we encounter an i/o timeout.
			if strings.Contains(err.Error(), "i/o timeout") {
				return nil
			}
			return err
		}
		// Calculate frame length.
		frameLength := int(binary.BigEndian.Uint32(append([]byte{0}, frame[:3]...)))
		if frameLength == 0 {
			continue
		}
		// Read the frame payload.
		payload := make([]byte, frameLength)
		_, err = c.Read(payload)
		if err != nil {
			// we want to stop reading from the socket when we encounter an i/o timeout.
			if strings.Contains(err.Error(), "i/o timeout") {
				return nil
			}
			return err
		}
	}
}

// headersWithNeverIndexedPath returns a set of header fields with path that never indexed.
func headersWithNeverIndexedPath() []hpack.HeaderField {
	return generateTestHeaderFields(true, false, false, "", "")
}

// headersWithoutIndexingPath returns a set of header fields with path without-indexing.
func headersWithoutIndexingPath() []hpack.HeaderField {
	return generateTestHeaderFields(false, true, false, "", "")
}

// headersWithGivenPathMethod returns a set of header fields with the given method for the header includes the path.
func headersWithGivenPathMethod(method string) []hpack.HeaderField {
	return generateTestHeaderFields(false, false, false, method, "")
}

// headersWithExceedingPathLen returns a set of header fields with a path that exceeds the maximum length.
func headersWithExceedingPathLen() []hpack.HeaderField {
	return generateTestHeaderFields(false, false, true, "", "")
}

// headersWithGivenEndpoint returns a set of header fields with the given path.
func headersWithGivenEndpoint(path string) []hpack.HeaderField {
	return generateTestHeaderFields(false, false, false, "", path)
}

// testHeaders returns a set of header fields.
func testHeaders() []hpack.HeaderField { return generateTestHeaderFields(false, false, false, "", "") }

// multipuleTestHeaders returns a set of header fields, with the given number of headers.
func multipuleTestHeaders(testHeadersCount int) []hpack.HeaderField {
	headers := generateTestHeaderFields(false, false, false, "", "")

	for i := 0; i < testHeadersCount; i++ {
		headers = append(headers, hpack.HeaderField{Name: fmt.Sprintf("name%d", i), Value: fmt.Sprintf("test%d", i)})
	}
	return headers
}

// generateTestHeaderFields generates a set of header fields that will be used for the tests.
func generateTestHeaderFields(pathNeverIndexed, withoutIndexing, pathExceedingMax bool, pathMethod, endpoint string) []hpack.HeaderField {
	headerPathMethod := "POST"
	path := http2DefaultTestPath
	if pathMethod != "" {
		headerPathMethod = pathMethod
	}
	if endpoint != "" {
		path = endpoint
	}
	pathHeaderField := hpack.HeaderField{Name: ":path", Value: path, Sensitive: false}

	// If we want to create a case without indexing, we need to make sure that the path is longer than 100 characters.
	// The indexing is determined by the dynamic table size (which we set to dynamicTableSize) and the size of the path.
	// ref: https://github.com/golang/net/blob/07e05fd6e95ab445ebe48840c81a027dbace3b8e/http2/hpack/encode.go#L140
	// Therefore, we want to make sure that the path is longer or equal to 100 characters so that the path will not be indexed.
	if withoutIndexing {
		pathHeaderField = hpack.HeaderField{Name: ":path", Value: "/" + strings.Repeat("a", usmhttp2.DynamicTableSize), Sensitive: true}
	}

	// If we want to create a case with a path that exceeds the maximum length,
	// we need to make sure that the path is longer than HTTP2_MAX_PATH_LEN.
	if pathExceedingMax {
		pathHeaderField = hpack.HeaderField{Name: ":path", Value: "/" + pathExceedingMaxSize, Sensitive: false}
	}

	// If the path is sensitive, we are in a case where a path is never indexed
	if pathNeverIndexed {
		pathHeaderField = hpack.HeaderField{Name: ":path", Value: http2DefaultTestPath, Sensitive: true}
	}

	return []hpack.HeaderField{
		{Name: ":authority", Value: authority},
		{Name: ":method", Value: headerPathMethod},
		pathHeaderField,
		{Name: ":scheme", Value: "http"},
		{Name: "content-type", Value: "application/json"},
		{Name: "content-length", Value: "4"},
		{Name: "accept-encoding", Value: "gzip"},
		{Name: "user-agent", Value: "Go-http-client/2.0"},
	}
}

// removeHeaderFieldByKey removes the header field with the given key from the given header fields.
func removeHeaderFieldByKey(headerFields []hpack.HeaderField, keyToRemove string) []hpack.HeaderField {
	for i := 0; i < len(headerFields); i++ {
		if headerFields[i].Name == keyToRemove {
			// Remove the element by modifying the slice in-place
			headerFields = append(headerFields[:i], headerFields[i+1:]...)
			break
		}
	}

	return headerFields
}

func getStreamID(streamID int) uint32 {
	return uint32(streamID*2 + 1)
}

type framer struct {
	buf    *bytes.Buffer
	framer *http2.Framer
}

func newFramer() *framer {
	buf := &bytes.Buffer{}
	return &framer{
		buf:    buf,
		framer: http2.NewFramer(buf, nil),
	}
}

func (f *framer) writeMultiMessage(t *testing.T, count int, cb func(t *testing.T) *framer) *framer {
	for i := 0; i < count; i++ {
		cb(t)
	}
	return f
}

func (f *framer) bytes() []byte {
	return f.buf.Bytes()
}

func (f *framer) writeSettings(t *testing.T) *framer {
	require.NoError(t, f.framer.WriteSettings(http2.Setting{}), "could not write settings frame")
	return f
}

func (f *framer) writePing(t *testing.T) *framer {
	require.NoError(t, f.framer.WritePing(true, [8]byte{}), "could not write ping frame")
	return f
}

func (f *framer) writeRSTStream(t *testing.T, streamID uint32, errCode http2.ErrCode) *framer {
	require.NoError(t, f.framer.WriteRSTStream(streamID, errCode), "could not write RST_STREAM")
	return f
}

func (f *framer) writeData(t *testing.T, streamID uint32, endStream bool, buf []byte) *framer {
	require.NoError(t, f.framer.WriteData(streamID, endStream, buf), "could not write data frame")
	return f
}

func (f *framer) writeWindowUpdate(t *testing.T, streamID uint32, increment uint32) *framer {
	require.NoError(t, f.framer.WriteWindowUpdate(streamID, increment), "could not write window update frame")
	return f
}

func (f *framer) writeRawHeaders(t *testing.T, streamID uint32, headerFrames []byte) *framer {
	require.NoError(t, f.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: headerFrames,
		EndHeaders:    true,
	}), "could not write header frames")
	return f
}

func (f *framer) writeHeaders(t *testing.T, streamID uint32, headerFields []hpack.HeaderField, setDynamicTableSize ...bool) *framer {
	changeDynamicTableSize := false
	if len(setDynamicTableSize) > 0 && setDynamicTableSize[0] {
		changeDynamicTableSize = true
	}
	headersFrame, err := usmhttp2.NewHeadersFrameMessage(headerFields, changeDynamicTableSize)
	require.NoError(t, err, "could not create headers frame")

	require.NoError(t, f.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: headersFrame,
		EndHeaders:    true,
	}), "could not write header frames")
	return f
}

type captureRange struct {
	lower int
	upper int
}

func getExpectedOutcomeForPathWithRepeatedChars() map[usmhttp.Key]captureRange {
	expected := make(map[usmhttp.Key]captureRange)
	for i := 1; i < 100; i++ {
		expected[usmhttp.Key{
			Path: usmhttp.Path{
				Content: usmhttp.Interner.GetString(fmt.Sprintf("/%s", strings.Repeat("a", i))),
			},
			Method: usmhttp.MethodPost,
		}] = captureRange{
			lower: 1,
			upper: 1,
		}
	}
	return expected
}

func startH2CServer(address string, isTLS bool) (func(), error) {
	srv := &http.Server{
		Addr: authority,
		Handler: h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check the request path and set the appropriate status code
			switch r.URL.Path {
			case pathWithStatusCode300:
				w.WriteHeader(http.StatusMultipleChoices) // HTTP status code 300
			case pathWithStatusCode401:
				w.WriteHeader(http.StatusUnauthorized) // HTTP status code 401
			default:
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("test"))
			}
		}), &http2.Server{}),
		IdleTimeout: 2 * time.Second,
	}

	if err := http2.ConfigureServer(srv, nil); err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	if isTLS {
		cert, key, err := testutil.GetCertsPaths()
		if err != nil {
			return nil, err
		}
		go srv.ServeTLS(l, cert, key)
	} else {
		go srv.Serve(l)
	}

	return func() {
		_ = srv.Shutdown(context.Background())
	}, nil
}

func getClientsIndex(index, totalCount int) int {
	return index % totalCount
}

// validateDynamicTableMap validates that the dynamic table map contains the expected indexes.
func validateDynamicTableMap(t *testing.T, ebpfProgram *ebpfProgram, expectedDynamicTablePathIndexes []int) {
	dynamicTableMap, _, err := ebpfProgram.GetMap("http2_dynamic_table")
	require.NoError(t, err)
	key := make([]byte, dynamicTableMap.KeySize())
	value := make([]byte, dynamicTableMap.ValueSize())
	iterator := dynamicTableMap.Iterate()
	resultIndexes := make([]int, 0)
	for iterator.Next(&key, &value) {
		tableIndex := usmhttp2.HTTP2DynamicTableIndex{}
		require.NoError(t, binary.Read(bytes.NewReader(key), binary.LittleEndian, &tableIndex))
		resultIndexes = append(resultIndexes, int(tableIndex.Index))
	}
	sort.Ints(resultIndexes)
	require.True(t, assert.EqualValues(t, expectedDynamicTablePathIndexes, resultIndexes))
}

// validateHuffmanEncoded validates that the dynamic table map contains the expected huffman encoded paths.
func validateHuffmanEncoded(t *testing.T, ebpfProgram *ebpfProgram, expectedHuffmanEncoded map[int]bool) {
	dynamicTableMap, _, err := ebpfProgram.GetMap("http2_dynamic_table")
	require.NoError(t, err)
	key := make([]byte, dynamicTableMap.KeySize())
	value := make([]byte, dynamicTableMap.ValueSize())
	iterator := dynamicTableMap.Iterate()
	resultEncodedPaths := make(map[int]bool, 0)
	for iterator.Next(&key, &value) {
		tableEntry := usmhttp2.HTTP2DynamicTableEntry{}
		require.NoError(t, binary.Read(bytes.NewReader(value), binary.LittleEndian, &tableEntry))
		resultEncodedPaths[int(tableEntry.String_len)] = tableEntry.Is_huffman_encoded
	}
	// we compare the size of the path and if it is huffman encoded.
	require.True(t, assert.EqualValues(t, expectedHuffmanEncoded, resultEncodedPaths))
}
