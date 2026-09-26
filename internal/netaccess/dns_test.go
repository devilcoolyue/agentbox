package netaccess

import (
	"context"
	"golang.org/x/net/dns/dnsmessage"
	"testing"
)

func TestDNSVirtualMappingAndAAAA(t *testing.T) {
	policy, err := Merge(Policy{}, []string{"git.corp:443"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Helper{policy: policy, ready: true}
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("GIT.corp."), Type: kind, Class: dnsmessage.ClassINET}}}
		raw, _ := q.Pack()
		reply, err := h.answerDNS(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		var result dnsmessage.Message
		if err = result.Unpack(reply); err != nil {
			t.Fatal(err)
		}
		if result.ID != 42 || !result.Response || result.RCode != dnsmessage.RCodeSuccess {
			t.Fatal(result)
		}
		if kind == dnsmessage.TypeA {
			if len(result.Answers) != 1 {
				t.Fatal(result)
			}
			a := result.Answers[0].Body.(*dnsmessage.AResource)
			if a.A != [4]byte{198, 18, 0, 1} {
				t.Fatal(a)
			}
		} else if len(result.Answers) != 0 {
			t.Fatal("IPv6 bypass")
		}
		h.ready = false
		reply, err = h.answerDNS(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		result.Unpack(reply)
		if result.RCode != dnsmessage.RCodeServerFailure {
			t.Fatal("unready helper answered DNS")
		}
		h.ready = true
	}
}
