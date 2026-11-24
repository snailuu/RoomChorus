package netutil

import (
	"bytes"
	"net"
	"testing"
)

func TestSubnetAndLoopbackChecks(t *testing.T) {
	_, lan, _ := net.ParseCIDR("192.168.1.0/24")

	if !IsLoopbackRemote("127.0.0.1:54321") {
		t.Fatal("expected 127.0.0.1 to be loopback")
	}
	if !IsLoopbackRemote("[::1]:54321") {
		t.Fatal("expected ::1 to be loopback")
	}
	if IsLoopbackRemote("192.168.1.22:54321") {
		t.Fatal("did not expect 192.168.1.22 to be loopback")
	}

	if !IsSameSubnetOrLoopback("192.168.1.88:1234", lan) {
		t.Fatal("expected same subnet IP to be allowed")
	}
	if !IsSameSubnetOrLoopback("127.0.0.1:1234", lan) {
		t.Fatal("expected loopback IP to be allowed")
	}
	if IsSameSubnetOrLoopback("192.168.2.88:1234", lan) {
		t.Fatal("did not expect different subnet IP to be allowed")
	}
}

func TestQRGeneration(t *testing.T) {
	pngBytes, err := GeneratePNG("http://192.168.1.10:8787/?k=abcd1234", 256)
	if err != nil {
		t.Fatalf("GeneratePNG failed: %v", err)
	}
	if !bytes.HasPrefix(pngBytes, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatal("expected valid PNG header")
	}
	ascii := TerminalQR("http://192.168.1.10:8787/?k=abcd1234")
	if len(ascii) < 20 {
		t.Fatal("expected non-empty terminal QR output")
	}
}
