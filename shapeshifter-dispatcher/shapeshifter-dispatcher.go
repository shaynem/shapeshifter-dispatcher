/*
 * Copyright (c) 2014-2015, Yawning Angel <yawning at torproject dot org>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *  * Redistributions of source code must retain the above copyright notice,
 *    this list of conditions and the following disclaimer.
 *
 *  * Redistributions in binary form must reproduce the above copyright notice,
 *    this list of conditions and the following disclaimer in the documentation
 *    and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// Go language Tor Pluggable Transport suite.  Works only as a managed
// client/server.
package main

import (
	"errors"
	"flag"
	"fmt"
	golog "log"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/shaynem/shapeshifter-dispatcher/common/log"
	"github.com/shaynem/shapeshifter-dispatcher/common/pt_extras"
	"github.com/shaynem/shapeshifter-dispatcher/common/termmon"
	"github.com/OperatorFoundation/shapeshifter-ipc"

	"github.com/shaynem/shapeshifter-dispatcher/modes/pt_socks5"
	"github.com/shaynem/shapeshifter-dispatcher/modes/stun_udp"
	"github.com/shaynem/shapeshifter-dispatcher/modes/transparent_tcp"
	"github.com/shaynem/shapeshifter-dispatcher/modes/transparent_udp"

	_ "github.com/shaynem/obfs4/proxy_dialers/proxy_http"
	_ "github.com/OperatorFoundation/obfs4/proxy_dialers/proxy_socks4"
	"github.com/shaynem/shapeshifter-dispatcher/transports"
	"github.com/shaynem/shapeshifter-transports/transports/base"
)

const (
	dispatcherVersion = "0.0.7-dev"
	dispatcherLogFile = "dispatcher.log"
)

var stateDir string
var termMon *termmon.TermMonitor

func getVersion() string {
	return fmt.Sprintf("dispatcher-%s", dispatcherVersion)
}

func main() {
	// Handle the command line arguments.
	_, execName := path.Split(os.Args[0])

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "shapeshifter-dispatcher is a PT v2.0 proxy supporting multiple transports and proxy modes\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n\t%s --client --state [statedir] --ptversion 2 --transports [transport1,transport2,...]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Example:\n\t%s --client --state state --ptversion 2 --transports obfs2\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Flags:\n\n")
		flag.PrintDefaults()
	}

	// PT 2.0 specification, 3.3.1.1. Common Configuration Parameters
	// FIXME: in the spec, this is -version, which is already used for printing the version number
	ptversion := flag.String("ptversion", "", "Specify the Pluggable Transport protocol version to use")
	statePath := flag.String("state", "", "Specify the directory to use to store state information required by the transports")
	exitOnStdinClose := flag.Bool("exit-on-stdin-close", false, "Set to true to force the dispatcher to close when the stdin pipe is closed")

	// NOTE: -transports is parsed as a common command line flag that overrides either TOR_PT_SERVER_TRANSPORTS or TOR_PT_CLIENT_TRANSPORTS
	transportsList := flag.String("transports", "", "Specify transports to enable")

	// PT 2.0 specification, 3.3.1.2. Pluggable PT Client Configuration Parameters
	proxy := flag.String("proxy", "", "Specify an HTTP or SOCKS4a proxy that the PT needs to use to reach the Internet")

	// PT 2.0 specification, 3.3.1.3. Pluggable PT Server Environment Variables
	options := flag.String("options", "", "Specify the transport options for the server")
	bindAddr := flag.String("bindaddr", "", "Specify the bind address for transparent server")
	orport := flag.String("orport", "", "Specify the address the server should forward traffic to in host:port format")
	extorport := flag.String("extorport", "", "Specify the address of a server implementing the Extended OR Port protocol, which is used for per-connection metadata")
	authcookie := flag.String("authcookie", "", "Specify an authentication cookie, for use in authenticating with the Extended OR Port")

	// Experimental flags under consideration for PT 2.1
	socksAddr := flag.String("proxylistenaddr", "127.0.0.1:0", "Specify the bind address for the local SOCKS server provided by the client")

	// Additional command line flags inherited from obfs4proxy
	showVer := flag.Bool("version", false, "Print version and exit")
	logLevelStr := flag.String("logLevel", "ERROR", "Log level (ERROR/WARN/INFO/DEBUG)")
	enableLogging := flag.Bool("enableLogging", false, "Log to TOR_PT_STATE_LOCATION/"+dispatcherLogFile)
	unsafeLogging := flag.Bool("unsafeLogging", false, "Disable the address scrubber")

	// Additional command line flags added to shapeshifter-dispatcher
	clientMode := flag.Bool("client", false, "Enable client mode")
	serverMode := flag.Bool("server", false, "Enable server mode")
	transparent := flag.Bool("transparent", false, "Enable transparent proxy mode. The default is protocol-aware proxy mode (SOCKS5 for TCP, STUN for UDP)")
	udp := flag.Bool("udp", false, "Enable UDP proxy mode. The default is TCP proxy mode.")
	target := flag.String("target", "", "Specify transport server destination address")
	flag.Parse()

	// Initialize the termination state monitor as soon as possible.
	termMon = termmon.NewTermMonitor(*exitOnStdinClose)

	if *showVer {
		fmt.Printf("%s\n", getVersion())
		os.Exit(0)
	}
	if err := log.SetLogLevel(*logLevelStr); err != nil {
		fmt.Println("failed to set log level")
		golog.Fatalf("[ERROR]: %s - failed to set log level: %s", execName, err)
	}

	// Determine if this is a client or server, initialize the common state.
	var clientListeners []net.Listener
	var serverListeners []base.TransportListener
	launched := false
	isClient, err := checkIsClient(*clientMode, *serverMode)
	if err != nil {
		flag.Usage()
		golog.Fatalf("[ERROR]: %s - either --client or --server is required, or configure using PT 2.0 environment variables", execName)
	}
	if stateDir, err = makeStateDir(*statePath); err != nil {
		flag.Usage()
		golog.Fatalf("[ERROR]: %s - No state directory: Use --state or TOR_PT_STATE_LOCATION environment variable", execName)
	}
	if err = log.Init(*enableLogging, path.Join(stateDir, dispatcherLogFile), *unsafeLogging); err != nil {
		golog.Fatalf("[ERROR]: %s - failed to initialize logging", execName)
	}

	log.Noticef("%s - launched", getVersion())

	if *transparent {
		// Do the transparent proxy configuration.
		log.Infof("%s - initializing transparent proxy", execName)
		if *udp {
			log.Infof("%s - initializing UDP transparent proxy", execName)
			if isClient {
				log.Infof("%s - initializing client transport listeners", execName)
				if *target == "" {
					log.Errorf("%s - transparent mode requires a target", execName)
				} else {
					ptClientProxy, names := getClientNames(ptversion, transportsList, proxy)

					launched = transparent_udp.ClientSetup(termMon, *socksAddr, *target, ptClientProxy, names, *options)
				}
			} else {
				log.Infof("%s - initializing server transport listeners", execName)
				if *bindAddr == "" {
					fmt.Errorf("%s - transparent mode requires a bindaddr", execName)
				} else {
					// launched = transparent_udp.ServerSetup(termMon, *bindAddr, *target)

					ptServerInfo := getServerInfo(ptversion, bindAddr, options, transportsList, orport, extorport, authcookie)
					launched, serverListeners = transparent_udp.ServerSetup(termMon, *bindAddr, ptServerInfo, *options)
				}
			}
		} else {
			log.Infof("%s - initializing TCP transparent proxy", execName)
			if isClient {
				log.Infof("%s - initializing client transport listeners", execName)
				if *target == "" {
					log.Errorf("%s - transparent mode requires a target", execName)
				} else {
					ptClientProxy, names := getClientNames(ptversion, transportsList, proxy)

					launched, clientListeners = transparent_tcp.ClientSetup(termMon, *socksAddr, *target, ptClientProxy, names, *options)
				}
			} else {
				log.Infof("%s - initializing server transport listeners", execName)
				if *bindAddr == "" {
					fmt.Errorf("%s - transparent mode requires a bindaddr", execName)
				} else {
					ptServerInfo := getServerInfo(ptversion, bindAddr, options, transportsList, orport, extorport, authcookie)
					launched, serverListeners = transparent_tcp.ServerSetup(termMon, *bindAddr, ptServerInfo, *statePath, *options)
				}
			}
		}
	} else {
		if *udp {
			log.Infof("%s - initializing STUN UDP proxy", execName)
			if isClient {
				log.Infof("%s - initializing client transport listeners", execName)
				if *target == "" {
					log.Errorf("%s - STUN mode requires a target", execName)
				} else {
					ptClientProxy, names := getClientNames(ptversion, transportsList, proxy)

					launched = stun_udp.ClientSetup(termMon, *socksAddr, *target, ptClientProxy, names, *options)
				}
			} else {
				log.Infof("%s - initializing server transport listeners", execName)
				if *bindAddr == "" {
					fmt.Errorf("%s - STUN mode requires a bindaddr", execName)
				} else {
					ptServerInfo := getServerInfo(ptversion, bindAddr, options, transportsList, orport, extorport, authcookie)
					launched, serverListeners = stun_udp.ServerSetup(termMon, *bindAddr, ptServerInfo, *options)
				}
			}
		} else {
			// Do the managed pluggable transport protocol configuration.
			log.Infof("%s - initializing PT 2.0 proxy", execName)
			if isClient {
				log.Infof("%s - initializing client transport listeners", execName)
				ptClientProxy, names := getClientNames(ptversion, transportsList, proxy)

				launched, clientListeners = pt_socks5.ClientSetup(termMon, *socksAddr, *target, ptClientProxy, names, *options)
			} else {
				log.Infof("%s - initializing server transport listeners", execName)
				ptServerInfo := getServerInfo(ptversion, bindAddr, options, transportsList, orport, extorport, authcookie)
				launched, serverListeners = pt_socks5.ServerSetup(termMon, *bindAddr, ptServerInfo, *options)
			}
		}
	}

	if !launched {
		// Initialization failed, the client or server setup routines should
		// have logged, so just exit here.
		os.Exit(-1)
	}

	log.Infof("%s - accepting connections", execName)
	defer func() {
		log.Noticef("%s - terminated", execName)
	}()

	// At this point, the pt config protocol is finished, and incoming
	// connections will be processed.  Wait till the parent dies
	// (immediate exit), a SIGTERM is received (immediate exit),
	// or a SIGINT is received.
	if sig := termMon.Wait(false); sig == syscall.SIGTERM {
		return
	}

	// Ok, it was the first SIGINT, close all listeners, and wait till,
	// the parent dies, all the current connections are closed, or either
	// a SIGINT/SIGTERM is received, and exit.
	for _, ln := range clientListeners {
		ln.Close()
	}

	for _, ln := range serverListeners {
		ln.Close()
	}

	termMon.Wait(true)

	for {
		// FIXME - block because termMon.Wait is not blocking
	}
}

func checkIsClient(client bool, server bool) (bool, error) {
	if client {
		return true, nil
	} else if server {
		return false, nil
	} else {
		return pt_extras.PtIsClient()
	}
}

func makeStateDir(statePath string) (string, error) {
	if statePath != "" {
		err := os.MkdirAll(statePath, 0700)
		return statePath, err
	} else {
		return pt.MakeStateDir()
	}
}

func getClientNames(ptversion *string, transportsList *string, proxy *string) (clientProxy *url.URL, names []string) {
	var ptClientInfo pt.ClientInfo
	var err error

	// FIXME - instead of this, goptlib should be modified to accept command line flag override of EITHER ptversion or transports (or both)
	if ptversion == nil || transportsList == nil {
		log.Infof("Falling back to environment variables for ptversion/transports %q %q", ptversion, transportsList)
		ptClientInfo, err = pt.ClientSetup(transports.Transports())
		if err != nil {
			// FIXME - print a more useful error, specifying --ptversion and --transports flags
			golog.Fatal(err)
		}
	} else {
		if *transportsList == "*" {
			ptClientInfo = pt.ClientInfo{MethodNames: transports.Transports()}
		} else {
			ptClientInfo = pt.ClientInfo{MethodNames: strings.Split(*transportsList, ",")}
		}
	}

	ptClientProxy, err := pt_extras.PtGetProxy(proxy)
	if err != nil {
		golog.Fatal(err)
	} else if ptClientProxy != nil {
		pt_extras.PtProxyDone()
	}

	return ptClientProxy, ptClientInfo.MethodNames
}

func getServerInfo(ptversion *string, bindaddrList *string, options *string, transportList *string, orport *string, extorport *string, authcookie *string) pt.ServerInfo {
	var ptServerInfo pt.ServerInfo
	var err error
	var bindaddrs []pt.Bindaddr

	bindaddrs, err = getServerBindaddrs(bindaddrList, options, transportList)
	if err != nil {
		fmt.Errorf("Error parsing bindaddrs %q %q %q", *bindaddrList, *options, *transportList)
		return ptServerInfo
	}

	ptServerInfo = pt.ServerInfo{Bindaddrs: bindaddrs}
	ptServerInfo.OrAddr, err = pt.ResolveAddr(*orport)
	if err != nil {
		fmt.Errorf("Error resolving OR address %q %q", orport, err)
		return ptServerInfo
	}

	if authcookie != nil {
		ptServerInfo.AuthCookiePath = *authcookie
	} else {
		ptServerInfo.AuthCookiePath = pt.Getenv("TOR_PT_AUTH_COOKIE_FILE")
	}

	if extorport != nil && *extorport != "" {
		ptServerInfo.ExtendedOrAddr, err = pt.ResolveAddr(*extorport)
		if err != nil {
			fmt.Errorf("Error resolving Extended OR address %q %q", *extorport, err)
			return ptServerInfo
		}
	} else {
		ptServerInfo.ExtendedOrAddr, err = pt.ResolveAddr(pt.Getenv("TOR_PT_EXTENDED_SERVER_PORT"))
		if err != nil {
			fmt.Errorf("Error resolving Extended OR address %q %q", err)
			return ptServerInfo
		}
	}

	return ptServerInfo
}

// Return an array of Bindaddrs, being the contents of TOR_PT_SERVER_BINDADDR
// with keys filtered by TOR_PT_SERVER_TRANSPORTS. Transport-specific options
// from TOR_PT_SERVER_TRANSPORT_OPTIONS are assigned to the Options member.
func getServerBindaddrs(bindaddrList *string, options *string, transports *string) ([]pt.Bindaddr, error) {
	var result []pt.Bindaddr
	var serverTransportOptions string
	var serverBindaddr string
	var serverTransports string
	var optionsMap map[string]pt.Args
	var err error

	// Parse the list of server transport options.
	if options == nil {
		serverTransportOptions = pt.Getenv("TOR_PT_SERVER_TRANSPORT_OPTIONS")
		if serverTransportOptions != "" {
			optionsMap, err = pt.ParseServerTransportOptions(serverTransportOptions)
			if err != nil {
				fmt.Errorf("Error parsing options map %q %q", serverTransportOptions, err)
				return nil, errors.New(fmt.Sprintf("TOR_PT_SERVER_TRANSPORT_OPTIONS: %q: %s", serverTransportOptions, err.Error()))
			}
		}
	} else {
		serverTransportOptions = *options
		if serverTransportOptions != "" {
			optionsMap, err = pt.ParsePT2ServerParameters(serverTransportOptions)
			if err != nil {
				fmt.Errorf("Error parsing options map %q %q", serverTransportOptions, err)
				return nil, errors.New(fmt.Sprintf("TOR_PT_SERVER_TRANSPORT_OPTIONS: %q: %s", serverTransportOptions, err.Error()))
			}
		}
	}

	// Get the list of all requested bindaddrs.
	if bindaddrList == nil {
		serverBindaddr, err = pt.GetenvRequired("TOR_PT_SERVER_BINDADDR")
		if err != nil {
			return nil, err
		}
	} else {
		serverBindaddr = *bindaddrList
	}
	for _, spec := range strings.Split(serverBindaddr, ",") {
		var bindaddr pt.Bindaddr

		parts := strings.SplitN(spec, "-", 2)
		if len(parts) != 2 {
			return nil, errors.New(fmt.Sprintf("TOR_PT_SERVER_BINDADDR: %q: doesn't contain \"-\"", spec))
		}
		bindaddr.MethodName = parts[0]
		addr, err := pt.ResolveAddr(parts[1])
		if err != nil {
			return nil, errors.New(fmt.Sprintf("TOR_PT_SERVER_BINDADDR: %q: %s", spec, err.Error()))
		}
		bindaddr.Addr = addr
		bindaddr.Options = optionsMap[bindaddr.MethodName]
		result = append(result, bindaddr)
	}

	// Filter by TOR_PT_SERVER_TRANSPORTS.
	if transports == nil {
		serverTransports, err = pt.GetenvRequired("TOR_PT_SERVER_TRANSPORTS")
		if err != nil {
			return nil, err
		}
	} else {
		serverTransports = *transports
	}
	result = pt.FilterBindaddrs(result, strings.Split(serverTransports, ","))

	return result, nil
}
