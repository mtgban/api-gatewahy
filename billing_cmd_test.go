package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mtgban/api-gatewahy/apiaccess"
	"github.com/mtgban/mtgban-website/apiproductlist"
)

func TestBillingUsageErrors(t *testing.T) {
	d := billingDeps{cat: apiproductlist.MustLoad()}
	cases := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"no verb", "checkout", nil, "usage: api-gatewahy checkout"},
		{"unknown verb", "checkout", []string{"pay"}, "usage: api-gatewahy checkout"},
		{"link without email", "checkout", []string{"link", "-package", "all_data"}, "-email is required"},
		{"link without package", "checkout", []string{"link", "-email", "x@example.com"}, "-package is required"},
		{"invite unknown interval", "invite", []string{"create", "-interval", "weekly"}, "unknown interval"},
		{"invite public interval", "invite", []string{"create", "-interval", "monthly"}, "does not need an invite"},
		{"portal without email", "portal", []string{"link"}, "-email is required"},
		{"plan without package", "plan", []string{"change", "-email", "x@example.com"}, "-package is required"},
		{"bad flag", "stripe", []string{"reconcile", "-bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := runBilling(context.Background(), d, c.cmd, c.args, &out, &errb); code != 2 && code != 1 {
			t.Errorf("%s: exit %d", c.name, code)
		}
		if !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: stderr %q lacks %q", c.name, errb.String(), c.want)
		}
	}
}

func TestStripeSubscriptionFor(t *testing.T) {
	stripeActive := apiaccess.Entitlement{Source: "stripe", Status: "active", ExternalRef: "sub_1"}
	stripeEnded := apiaccess.Entitlement{Source: "stripe", Status: "ended", ExternalRef: "sub_0"}
	manual := apiaccess.Entitlement{Source: "manual", Status: "active"}
	if id, err := stripeSubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded, stripeActive}); err != nil || id != "sub_1" {
		t.Errorf("one live: %q %v", id, err)
	}
	if _, err := stripeSubscriptionFor([]apiaccess.Entitlement{manual, stripeEnded}); err == nil {
		t.Error("none live accepted")
	}
	other := stripeActive
	other.ExternalRef = "sub_2"
	if _, err := stripeSubscriptionFor([]apiaccess.Entitlement{stripeActive, other}); err == nil || !strings.Contains(err.Error(), "-sub") {
		t.Errorf("two live: %v", err)
	}
}

func TestBillingCommandsRegistered(t *testing.T) {
	for _, name := range []string{"catalog", "checkout", "invite", "stripe", "portal", "plan"} {
		if _, ok := commands[name]; !ok {
			t.Errorf("command %s not registered", name)
		}
	}
}
