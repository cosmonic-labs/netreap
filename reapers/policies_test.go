package reapers

import (
	"encoding/json"
	"testing"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/policy/api"
	"go.uber.org/zap"
)

type testPolicyRepository struct {
	rules api.Rules
}

func (p *testPolicyRepository) PolicyGet(lbls []string) (*models.Policy, error) {
	result := api.Rules{}

	toGet := labels.ParseLabelArray(lbls...)

	for _, r := range p.rules {
		if r.Labels.Contains(toGet) {
			result = append(result, r)
		}
	}

	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}

	return &models.Policy{
		Policy: string(b),
	}, nil
}

func (p *testPolicyRepository) PolicyReplace(policyJSON string, replace bool, replaceWithLabels []string) (*models.Policy, error) {
	var newRules api.Rules

	err := json.Unmarshal([]byte(policyJSON), &newRules)
	if err != nil {
		return nil, err
	}

	if replace {
		for _, r := range newRules {
			oldRules := p.search(r.Labels)
			if len(oldRules) > 0 {
				p.delete(r.Labels)
			}

		}
	}
	if len(replaceWithLabels) > 0 {
		lbls := labels.ParseLabelArray(replaceWithLabels...)
		oldRules := p.search(lbls)
		if len(oldRules) > 0 {
			p.delete(lbls)
		}

	}

	p.add(newRules)

	return &models.Policy{}, nil
}

func (p *testPolicyRepository) PolicyDelete(lbls []string) (*models.Policy, error) {
	toDelete := labels.ParseLabelArray(lbls...)

	err := p.delete(toDelete)
	if err != nil {
		return nil, err
	}

	return &models.Policy{}, nil
}

func (p *testPolicyRepository) add(rules api.Rules) {
	p.rules = append(p.rules, rules...)
}

func (p *testPolicyRepository) search(toFind labels.LabelArray) api.Rules {
	result := api.Rules{}

	for _, r := range p.rules {
		if r.Labels.Contains(toFind) {
			result = append(result, r)
		}
	}

	return result
}

func (p *testPolicyRepository) delete(toDelete labels.LabelArray) error {
	deleted := 0
	new := p.rules[:0]

	for _, r := range p.rules {
		if !r.Labels.Contains(toDelete) {
			new = append(new, r)
		} else {
			deleted++
		}
	}

	if deleted > 0 {
		p.rules = new
	}

	return nil
}

func TestGetCurrentPolicies(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	// Test empty
	current, err := policiesReaper.getCurrentPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("expected empty policy set, got %v", current)
	}

	// Test one with label
	cilium.add(api.Rules{
		api.NewRule().
			WithLabels(getIdentityLabels("one")).
			WithEndpointSelector(api.EndpointSelectorNone),
	})
	current, err = policiesReaper.getCurrentPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 {
		t.Fatalf("expected one policy set, got %v", current)
	}
	if current["one"][0].Labels[0].Value != "one" {
		t.Fatalf("expected one policy set with label 'one', got %v", current)
	}
}

func TestCreatePolicyEvent(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	one := api.Rules{
		api.NewRule().
			WithDescription("one").
			WithEndpointSelector(api.EndpointSelectorNone),
	}

	oneValue, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}

	err = policiesReaper.handlePolicyEvent(zap.L(), "one", kvstore.KeyValueEvent{
		Typ:   kvstore.EventTypeCreate,
		Key:   "one",
		Value: oneValue,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cilium.rules) != 1 {
		t.Fatalf("Failed to add rule")
	}

	if cilium.rules[0].Description != one[0].Description {
		t.Fatalf("Failed to add correct rule, got %v", cilium.rules[0])
	}
}

func TestModifyPolicyEvent(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	one := api.Rules{
		api.NewRule().
			WithDescription("one - modified").
			WithEndpointSelector(api.EndpointSelectorNone),
	}

	oneValue, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}

	cilium.add(api.Rules{
		api.NewRule().
			WithDescription("one").
			WithLabels(getIdentityLabels("one")).
			WithEndpointSelector(api.EndpointSelectorNone),
		api.NewRule().
			WithDescription("two").
			WithLabels(getIdentityLabels("two")).
			WithEndpointSelector(api.EndpointSelectorNone),
	})

	err = policiesReaper.handlePolicyEvent(zap.L(), "one", kvstore.KeyValueEvent{
		Typ:   kvstore.EventTypeModify,
		Key:   "one",
		Value: oneValue,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cilium.rules) != 2 {
		t.Fatalf("Should not have changed rule store size")
	}

	// Modified rules get added to the end
	if cilium.rules[1].Description != "one - modified" {
		t.Fatalf("Failed to modify correct rule")
	}
}

func TestModifyPolicyEventNoDifference(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	one := api.Rules{
		api.NewRule().
			WithDescription("one").
			WithEndpointSelector(api.EndpointSelectorNone),
	}

	oneValue, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}

	cilium.add(api.Rules{
		api.NewRule().
			WithDescription("one").
			WithLabels(getIdentityLabels("one")).
			WithEndpointSelector(api.EndpointSelectorNone),
		api.NewRule().
			WithDescription("two").
			WithLabels(getIdentityLabels("two")).
			WithEndpointSelector(api.EndpointSelectorNone),
	})

	err = policiesReaper.handlePolicyEvent(zap.L(), "one", kvstore.KeyValueEvent{
		Typ:   kvstore.EventTypeModify,
		Key:   "one",
		Value: oneValue,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cilium.rules) != 2 {
		t.Fatalf("Should not have changed rule store size")
	}

	// Should still be in the same order
	if cilium.rules[0].Description != "one" || cilium.rules[1].Description != "two" {
		t.Fatalf("Rule store was mutated")
	}
}

func TestDeletePolicyEvent(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	cilium.add(api.Rules{
		api.NewRule().
			WithDescription("one").
			WithLabels(getIdentityLabels("one")).
			WithEndpointSelector(api.EndpointSelectorNone),
		api.NewRule().
			WithDescription("two").
			WithLabels(getIdentityLabels("two")).
			WithEndpointSelector(api.EndpointSelectorNone),
	})

	err = policiesReaper.handlePolicyEvent(zap.L(), "one", kvstore.KeyValueEvent{
		Typ: kvstore.EventTypeDelete,
		Key: "one",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cilium.rules) != 1 {
		t.Fatalf("Failed to delete rule")
	}

	if cilium.rules[0].Description != "two" {
		t.Fatalf("Failed to delete correct rule")
	}
}

func TestSyncDeleteOrphan(t *testing.T) {
	cilium := &testPolicyRepository{
		rules: make(api.Rules, 0),
	}

	policiesReaper, err := NewPoliciesReaper(nil, "", cilium)
	if err != nil {
		t.Fatal(err)
	}

	one := api.Rules{
		api.NewRule().
			WithDescription("one").
			WithEndpointSelector(api.EndpointSelectorNone),
	}

	oneValue, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}

	two := api.Rules{
		api.NewRule().
			WithDescription("two").
			WithEndpointSelector(api.EndpointSelectorNone),
	}

	twoValue, err := json.Marshal(two)
	if err != nil {
		t.Fatal(err)
	}

	oldRules := api.Rules{
		api.NewRule().
			WithDescription("three").
			WithLabels(getIdentityLabels("three")).
			WithEndpointSelector(api.EndpointSelectorNone),
	}
	oldPolicies := make(map[string]api.Rules)
	oldPolicies["three"] = oldRules

	cilium.add(oldRules)

	err = policiesReaper.handleSyncPolicyEvent(zap.L(), oldPolicies, kvstore.KeyValueEvent{
		Typ:   kvstore.EventTypeCreate,
		Key:   "one",
		Value: oneValue,
	})
	if err != nil {
		t.Fatal(err)
	}

	policiesReaper.handleSyncPolicyEvent(zap.L(), oldPolicies, kvstore.KeyValueEvent{
		Typ:   kvstore.EventTypeCreate,
		Key:   "two",
		Value: twoValue,
	})
	if err != nil {
		t.Fatal(err)
	}

	policiesReaper.handleSyncPolicyEvent(zap.L(), oldPolicies, kvstore.KeyValueEvent{
		Typ: kvstore.EventTypeListDone,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(cilium.rules) != 2 {
		t.Fatalf("Failed to delete orphaned rule")
	}

	if cilium.rules[0].Description != "one" {
		t.Fatalf("Expected rule 'one', got : %v", cilium.rules[0])
	}

	if cilium.rules[1].Description != "two" {
		t.Fatalf("Expected rule 'two', got : %v", cilium.rules[1])
	}

}
