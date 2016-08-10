package vault

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hashicorp/vault/helper/jsonutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/physical"
)

func TestCluster(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)
	cluster, err := c.Cluster()
	if err != nil {
		t.Fatal(err)
	}
	// Test whether expected values are found
	if cluster == nil || cluster.Name == "" || cluster.ID == "" {
		t.Fatalf("cluster information missing: cluster: %#v", cluster)
	}

	// Test whether a private key has been generated
	entry, err := c.barrier.Get(coreLocalClusterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("missing local cluster private key")
	}

	var params privKeyParams
	if err = jsonutil.DecodeJSON(entry.Value, &params); err != nil {
		t.Fatal(err)
	}
	switch {
	case params.X == nil, params.Y == nil, params.D == nil:
		t.Fatalf("x or y or d are nil: %#v", params)
	case params.Type == corePrivateKeyTypeP521:
	default:
		t.Fatal("parameter error: %#v", params)
	}
}

func TestClusterHA(t *testing.T) {
	logger = log.New(os.Stderr, "", log.LstdFlags)
	advertise := "http://127.0.0.1:8200"

	c, err := NewCore(&CoreConfig{
		Physical:      physical.NewInmemHA(logger),
		HAPhysical:    physical.NewInmemHA(logger),
		AdvertiseAddr: advertise,
		DisableMlock:  true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	key, _ := TestCoreInit(t, c)
	if _, err := TestCoreUnseal(c, TestKeyCopy(key)); err != nil {
		t.Fatalf("unseal err: %s", err)
	}

	// Verify unsealed
	sealed, err := c.Sealed()
	if err != nil {
		t.Fatalf("err checking seal status: %s", err)
	}
	if sealed {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	testWaitActive(t, c)

	cluster, err := c.Cluster()
	if err != nil {
		t.Fatal(err)
	}
	// Test whether expected values are found
	if cluster == nil || cluster.Name == "" || cluster.ID == "" || cluster.Certificate == nil || len(cluster.Certificate) == 0 {
		t.Fatalf("cluster information missing: cluster:%#v", cluster)
	}

	// Test whether a private key has been generated
	entry, err := c.barrier.Get(coreLocalClusterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil {
		t.Fatal("missing local cluster private key")
	}

	var params privKeyParams
	if err = jsonutil.DecodeJSON(entry.Value, &params); err != nil {
		t.Fatal(err)
	}
	switch {
	case params.X == nil, params.Y == nil, params.D == nil:
		t.Fatalf("x or y or d are nil: %#v", params)
	case params.Type == corePrivateKeyTypeP521:
	default:
		t.Fatal("parameter error: %#v", params)
	}

	// Make sure the certificate meets expectations
	cert, err := x509.ParseCertificate(cluster.Certificate)
	if err != nil {
		t.Fatal("error parsing local cluster certificate: %v", err)
	}

	// Make sure the cert pool is as expected
	if len(c.localClusterCertPool.Subjects()) != 1 {
		t.Fatal("unexpected local cluster cert pool length")
	}
	if !reflect.DeepEqual(cert.RawSubject, c.localClusterCertPool.Subjects()[0]) {
		t.Fatal("cert pool subject does not match expected")
	}
}

func TestCluster_ForwardCommon(t *testing.T) {
	logger = log.New(os.Stderr, "", log.LstdFlags)

	logicalBackends := make(map[string]logical.Factory)
	logicalBackends["generic"] = PassthroughBackendFactory

	// Create two cores with the same physical and different advertise addrs
	coreConfig := &CoreConfig{
		Physical:        physical.NewInmem(logger),
		HAPhysical:      physical.NewInmemHA(logger),
		LogicalBackends: logicalBackends,
		AdvertiseAddr:   "https://127.0.0.1:8202",
		DisableMlock:    true,
	}
	c1, err := NewCore(coreConfig)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	coreConfig.AdvertiseAddr = "https://127.0.0.1:8206"
	c2, err := NewCore(coreConfig)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:8202")
	if err != nil {
		t.Fatal(err)
	}
	c1lns := []net.Listener{ln}
	ln, err = net.Listen("tcp", "127.0.0.1:8204")
	if err != nil {
		t.Fatal(err)
	}
	c1lns = append(c1lns, ln)
	ln, err = net.Listen("tcp", "127.0.0.1:8206")
	if err != nil {
		t.Fatal(err)
	}
	c2lns := []net.Listener{ln}

	defer func() {
		for _, ln := range c1lns {
			ln.Close()
		}
		for _, ln := range c2lns {
			ln.Close()
		}
	}()

	clusterListenerSetupFunc := func(c *Core, lns []net.Listener) ([]net.Listener, http.Handler, error) {
		ret := make([]net.Listener, 0, len(lns))
		// Loop over the existing listeners and start listeners on appropriate ports
		for _, ln := range lns {
			tcpAddr, ok := ln.Addr().(*net.TCPAddr)
			if !ok {
				c.logger.Printf("[TRACE] command/server: %s not a candidate for cluster request handling", ln.Addr().String())
				continue
			}
			c.logger.Printf("[TRACE] command/server: %s is a candidate for cluster request handling at addr %s and port %d", tcpAddr.String(), tcpAddr.IP.String(), tcpAddr.Port+1)

			ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", tcpAddr.IP.String(), tcpAddr.Port+1))
			if err != nil {
				return nil, nil, err
			}
			ret = append(ret, ln)
		}

		return ret, nil, nil
	}

	c1SetupFunc := func() ([]net.Listener, http.Handler, error) {
		return clusterListenerSetupFunc(c1, c1lns)
	}
	c2SetupFunc := func() ([]net.Listener, http.Handler, error) {
		return clusterListenerSetupFunc(c1, c2lns)
	}

	c2.SetClusterListenerSetupFunc(c2SetupFunc)

	key, root := TestCoreInitClusterListenerSetup(t, c1, c1SetupFunc)
	if _, err := c1.Unseal(TestKeyCopy(key)); err != nil {
		t.Fatalf("unseal err: %s", err)
	}

	// Verify unsealed
	sealed, err := c1.Sealed()
	if err != nil {
		t.Fatalf("err checking seal status: %s", err)
	}
	if sealed {
		t.Fatal("should not be sealed")
	}

	// Make this nicer for tests
	oldManualStepDownSleepPeriod := manualStepDownSleepPeriod
	manualStepDownSleepPeriod = 3 * time.Second
	// Restore this value for other tests
	defer func() { manualStepDownSleepPeriod = oldManualStepDownSleepPeriod }()

	// Wait for core to become active
	testWaitActive(t, c1)

	// At this point c2 should still be sealed. We don't want to have more than
	// one core unsealed for the listener tests since we do some timing with
	// step-downs.
	testCluster_ListenForRequests(t, c1, c1lns, root)

	// Re-unseal core1, wait for it to be active, then unseal core2.
	if _, err := c1.Unseal(TestKeyCopy(key)); err != nil {
		t.Fatalf("unseal err: %s", err)
	}
	testWaitActive(t, c1)
	if _, err := c2.Unseal(TestKeyCopy(key)); err != nil {
		t.Fatalf("unseal err: %s", err)
	}

	// Test forwarding a request. Since we're going directly from core to core
	// with no fallback we know that if it worked, request handling is working
	testCluster_ForwardRequests(t, c1, c2, root)
}

func testCluster_ListenForRequests(t *testing.T, c *Core, lns []net.Listener, root string) {
	tlsConfig, err := c.ClusterTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	checkListenersFunc := func(expectFail bool) {
		for _, ln := range lns {
			tcpAddr, ok := ln.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatal("%s not a TCP port", tcpAddr.String())
			}

			conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", tcpAddr.IP.String(), tcpAddr.Port+1), tlsConfig)
			if err != nil {
				if expectFail {
					t.Logf("testing %s:%d unsuccessful as expected", tcpAddr.IP.String(), tcpAddr.Port+1)
					continue
				}
				t.Fatalf("error: %v\nlisteners are\n%#v\n%#v\n", err, lns[0].(*net.TCPListener).Addr(), lns[1].(*net.TCPListener).Addr())
			}
			if expectFail {
				t.Fatalf("testing %s:%d not unsuccessful as expected", tcpAddr.IP.String(), tcpAddr.Port+1)
			}
			err = conn.Handshake()
			if err != nil {
				t.Fatal(err)
			}
			connState := conn.ConnectionState()
			switch {
			case connState.Version != tls.VersionTLS12:
				t.Fatal("version mismatch")
			case connState.NegotiatedProtocol != "h2" || !connState.NegotiatedProtocolIsMutual:
				t.Fatal("bad protocol negotiation")
			}
			t.Logf("testing %s:%d successful", tcpAddr.IP.String(), tcpAddr.Port+1)
		}
	}

	checkListenersFunc(false)

	err = c.StepDown(&logical.Request{
		Operation:   logical.UpdateOperation,
		Path:        "sys/step-down",
		ClientToken: root,
	})
	if err != nil {
		t.Fatal(err)
	}

	// StepDown doesn't wait during actual preSeal so give time for listeners
	// to close
	time.Sleep(1 * time.Second)
	checkListenersFunc(true)

	time.Sleep(manualStepDownSleepPeriod)
	checkListenersFunc(false)

	err = c.Seal(root)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1 * time.Second)
	checkListenersFunc(true)
}

func testCluster_ForwardRequests(t *testing.T, c1 *Core, c2 *Core, root string) {
	testWaitActive(t, c1)
}
