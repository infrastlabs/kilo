// Copyright 2019 the Kilo authors
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

package iptables

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-iptables/iptables"
)

type iptablesClient interface {
	AppendUnique(string, string, ...string) error
	Delete(string, string, ...string) error
	Exists(string, string, ...string) (bool, error)
	ClearChain(string, string) error
	DeleteChain(string, string) error
	NewChain(string, string) error
}

// rule represents an iptables rule.
type rule struct {
	table  string
	chain  string
	spec   []string
	client iptablesClient
}

func (r *rule) Add() error {
	if err := r.client.AppendUnique(r.table, r.chain, r.spec...); err != nil {
		return fmt.Errorf("failed to add iptables rule: %v", err)
	}
	return nil
}

func (r *rule) Delete() error {
	// Ignore the returned error as an error likely means
	// that the rule doesn't exist, which is fine.
	r.client.Delete(r.table, r.chain, r.spec...)
	return nil
}

func (r *rule) Exists() (bool, error) {
	return r.client.Exists(r.table, r.chain, r.spec...)
}

func (r *rule) String() string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%s_%s_%s", r.table, r.chain, strings.Join(r.spec, "_"))
}

// chain represents an iptables chain.
type chain struct {
	table  string
	chain  string
	client iptablesClient
}

func (c *chain) Add() error {
	if err := c.client.ClearChain(c.table, c.chain); err != nil {
		return fmt.Errorf("failed to add iptables chain: %v", err)
	}
	return nil
}

func (c *chain) Delete() error {
	// The chain must be empty before it can be deleted.
	if err := c.client.ClearChain(c.table, c.chain); err != nil {
		return fmt.Errorf("failed to clear iptables chain: %v", err)
	}
	// Ignore the returned error as an error likely means
	// that the chain doesn't exist, which is fine.
	c.client.DeleteChain(c.table, c.chain)
	return nil
}

func (c *chain) Exists() (bool, error) {
	// The code for "chain already exists".
	existsErr := 1
	err := c.client.NewChain(c.table, c.chain)
	se, ok := err.(statusExiter)
	switch {
	case err == nil:
		// If there was no error adding a new chain, then it did not exist.
		// Delete it and return false.
		c.client.DeleteChain(c.table, c.chain)
		return false, nil
	case ok && se.ExitStatus() == existsErr:
		return true, nil
	default:
		return false, err
	}
}

func (c *chain) String() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s_%s", c.table, c.chain)
}

// Rule is an interface for interacting with iptables objects.
type Rule interface {
	Add() error
	Delete() error
	Exists() (bool, error)
	String() string
}

// Controller is able to reconcile a given set of iptables rules.
type Controller struct {
	client iptablesClient
	errors chan error

	sync.Mutex
	rules      []Rule
	subscribed bool
}

// New generates a new iptables rules controller.
// It expects an IP address length to determine
// whether to operate in IPv4 or IPv6 mode.
func New(ipLength int) (*Controller, error) {
	p := iptables.ProtocolIPv4
	if ipLength == net.IPv6len {
		p = iptables.ProtocolIPv6
	}
	client, err := iptables.NewWithProtocol(p)
	if err != nil {
		return nil, fmt.Errorf("failed to create iptables client: %v", err)
	}
	return &Controller{
		client: client,
		errors: make(chan error),
	}, nil
}

// Run watches for changes to iptables rules and reconciles
// the rules against the desired state.
func (c *Controller) Run(stop <-chan struct{}) (<-chan error, error) {
	c.Lock()
	if c.subscribed {
		c.Unlock()
		return c.errors, nil
	}
	// Ensure a given instance only subscribes once.
	c.subscribed = true
	c.Unlock()
	go func() {
		defer close(c.errors)
		for {
			select {
			case <-time.After(5 * time.Second):
			case <-stop:
				return
			}
			if err := c.reconcile(); err != nil {
				nonBlockingSend(c.errors, fmt.Errorf("failed to reconcile rules: %v", err))
			}
		}
	}()
	return c.errors, nil
}

// reconcile makes sure that every rule is still in the backend.
// It does not ensure that the order in the backend is correct.
// If any rule is missing, that rule and all following rules are
// re-added.
func (c *Controller) reconcile() error {
	c.Lock()
	defer c.Unlock()
	for i, r := range c.rules {
		ok, err := r.Exists()
		if err != nil {
			return fmt.Errorf("failed to check if rule exists: %v", err)
		}
		if !ok {
			if err := resetFromIndex(i, c.rules); err != nil {
				return fmt.Errorf("failed to add rule: %v", err)
			}
			break
		}
	}
	return nil
}

// resetFromIndex re-adds all rules starting from the given index.
func resetFromIndex(i int, rules []Rule) error {
	if i >= len(rules) {
		return nil
	}
	for j := i; j < len(rules); j++ {
		if err := rules[j].Delete(); err != nil {
			return fmt.Errorf("failed to delete rule: %v", err)
		}
		if err := rules[j].Add(); err != nil {
			return fmt.Errorf("failed to add rule: %v", err)
		}
	}
	return nil
}

// deleteFromIndex deletes all rules starting from the given index.
func deleteFromIndex(i int, rules *[]Rule) error {
	if i >= len(*rules) {
		return nil
	}
	for j := i; j < len(*rules); j++ {
		if err := (*rules)[j].Delete(); err != nil {
			return fmt.Errorf("failed to delete rule: %v", err)
		}
		(*rules)[j] = nil
	}
	*rules = (*rules)[:i]
	return nil
}

// Set idempotently overwrites any iptables rules previously defined
// for the controller with the given set of rules.
func (c *Controller) Set(rules []Rule) error {
	c.Lock()
	defer c.Unlock()
	var i int
	for ; i < len(rules); i++ {
		if i < len(c.rules) {
			if rules[i].String() != c.rules[i].String() {
				if err := deleteFromIndex(i, &c.rules); err != nil {
					return err
				}
			}
		}
		if i >= len(c.rules) {
			setRuleClient(rules[i], c.client)
			if err := rules[i].Add(); err != nil {
				return fmt.Errorf("failed to add rule: %v", err)
			}
			c.rules = append(c.rules, rules[i])
		}

	}
	return deleteFromIndex(i, &c.rules)
}

// CleanUp will clean up any rules created by the controller.
func (c *Controller) CleanUp() error {
	c.Lock()
	defer c.Unlock()
	return deleteFromIndex(0, &c.rules)
}

// IPIPRules returns a set of iptables rules that are necessary
// when traffic between nodes must be encapsulated with IPIP.
func IPIPRules(nodes []*net.IPNet) []Rule {
	var rules []Rule
	rules = append(rules, &chain{"filter", "KILO-IPIP", nil})
	rules = append(rules, &rule{"filter", "INPUT", []string{"-m", "comment", "--comment", "Kilo: jump to IPIP chain", "-p", "4", "-j", "KILO-IPIP"}, nil})
	for _, n := range nodes {
		// Accept encapsulated traffic from peers.
		rules = append(rules, &rule{"filter", "KILO-IPIP", []string{"-m", "comment", "--comment", "Kilo: allow IPIP traffic", "-s", n.IP.String(), "-j", "ACCEPT"}, nil})
	}
	// Drop all other IPIP traffic.
	rules = append(rules, &rule{"filter", "INPUT", []string{"-m", "comment", "--comment", "Kilo: reject other IPIP traffic", "-p", "4", "-j", "DROP"}, nil})

	return rules
}

// ForwardRules returns a set of iptables rules that are necessary
// when traffic must be forwarded for the overlay.
func ForwardRules(subnets ...*net.IPNet) []Rule {
	var rules []Rule
	for _, subnet := range subnets {
		s := subnet.String()
		rules = append(rules, []Rule{
			// Forward traffic to and from the overlay.
			&rule{"filter", "FORWARD", []string{"-s", s, "-j", "ACCEPT"}, nil},
			&rule{"filter", "FORWARD", []string{"-d", s, "-j", "ACCEPT"}, nil},
		}...)
	}
	return rules
}

// MasqueradeRules returns a set of iptables rules that are necessary
// to NAT traffic from the local Pod subnet to the Internet and out of the Kilo interface.
func MasqueradeRules(kilo, private, localPodSubnet *net.IPNet, remotePodSubnet, peers []*net.IPNet) []Rule {
	var rules []Rule
	rules = append(rules, &chain{"nat", "KILO-NAT", nil})

	// NAT packets from Kilo interface.
	rules = append(rules, &rule{"mangle", "PREROUTING", []string{"-m", "comment", "--comment", "Kilo: jump to mark chain", "-i", "kilo+", "-j", "MARK", "--set-xmark", "0x1107/0x1107"}, nil})
	rules = append(rules, &rule{"nat", "POSTROUTING", []string{"-m", "comment", "--comment", "Kilo: NAT packets from Kilo interface", "-m", "mark", "--mark", "0x1107/0x1107", "-j", "KILO-NAT"}, nil})

	// NAT packets from pod subnet.
	rules = append(rules, &rule{"nat", "POSTROUTING", []string{"-m", "comment", "--comment", "Kilo: jump to NAT chain", "-s", localPodSubnet.String(), "-j", "KILO-NAT"}, nil})
	rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: do not NAT packets destined for the local Pod subnet", "-d", localPodSubnet.String(), "-j", "RETURN"}, nil})
	rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: do not NAT packets destined for the Kilo subnet", "-d", kilo.String(), "-j", "RETURN"}, nil})
	rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: do not NAT packets destined for the local private IP", "-d", private.String(), "-j", "RETURN"}, nil})
	for _, r := range remotePodSubnet {
		rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: do not NAT packets from local pod subnet to remote pod subnets", "-s", localPodSubnet.String(), "-d", r.String(), "-j", "RETURN"}, nil})
	}
	for _, p := range peers {
		rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: do not NAT packets from local pod subnet to peers", "-s", localPodSubnet.String(), "-d", p.String(), "-j", "RETURN"}, nil})
	}
	rules = append(rules, &rule{"nat", "KILO-NAT", []string{"-m", "comment", "--comment", "Kilo: NAT remaining packets", "-j", "MASQUERADE"}, nil})
	return rules
}

func nonBlockingSend(errors chan<- error, err error) {
	select {
	case errors <- err:
	default:
	}
}

// setRuleClient is a helper to set the iptables client on different kinds of rules.
func setRuleClient(r Rule, c iptablesClient) {
	switch v := r.(type) {
	case *rule:
		v.client = c
	case *chain:
		v.client = c
	}
}
