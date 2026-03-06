// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package rule

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/ory/oathkeeper/driver/configuration"
	"github.com/ory/oathkeeper/helper"

	"github.com/ory/x/logrusx"
	"github.com/ory/x/pagination"
)

var _ Repository = new(RepositoryMemory)

type repositoryMemoryRegistry interface {
	RuleValidator() Validator
	logrusx.Provider
}

type RepositoryMemory struct {
	sync.RWMutex
	rules            []Rule
	invalidRules     []Rule
	matchingStrategy configuration.MatchingStrategy
	r                repositoryMemoryRegistry
}

// MatchingStrategy returns current MatchingStrategy.
func (m *RepositoryMemory) MatchingStrategy(_ context.Context) (configuration.MatchingStrategy, error) {
	m.RLock()
	defer m.RUnlock()
	return m.matchingStrategy, nil
}

// SetMatchingStrategy updates MatchingStrategy.
func (m *RepositoryMemory) SetMatchingStrategy(_ context.Context, ms configuration.MatchingStrategy) error {
	m.Lock()
	defer m.Unlock()
	m.matchingStrategy = ms
	return nil
}

func NewRepositoryMemory(r repositoryMemoryRegistry) *RepositoryMemory {
	return &RepositoryMemory{
		r:     r,
		rules: make([]Rule, 0),
	}
}

// WithRules sets rules without validation. For testing only.
func (m *RepositoryMemory) WithRules(rules []Rule) {
	m.Lock()
	m.rules = rules
	m.Unlock()
}

func (m *RepositoryMemory) Count(ctx context.Context) (int, error) {
	m.RLock()
	defer m.RUnlock()

	return len(m.rules), nil
}

func (m *RepositoryMemory) List(ctx context.Context, limit, offset int) ([]Rule, error) {
	m.RLock()
	defer m.RUnlock()

	start, end := pagination.Index(limit, offset, len(m.rules))
	return m.rules[start:end], nil
}

func (m *RepositoryMemory) Get(ctx context.Context, id string) (*Rule, error) {
	m.RLock()
	defer m.RUnlock()

	for _, r := range m.rules {
		if r.ID == id {
			return &r, nil
		}
	}

	return nil, errors.WithStack(helper.ErrResourceNotFound)
}

func (m *RepositoryMemory) Set(ctx context.Context, rules []Rule) error {
	m.Lock()
	defer m.Unlock()

	m.rules = make([]Rule, 0, len(rules))
	m.invalidRules = make([]Rule, 0)

	for _, check := range rules {
		if err := m.r.RuleValidator().Validate(&check); err != nil {
			matchURL := "<no match configured>"
			if check.Match != nil {
				matchURL = check.Match.GetURL()
			}
			m.r.Logger().WithError(err).
				WithField("rule_id", check.ID).
				WithField("match_url", matchURL).
				Errorf("Rule %q is invalid and will be skipped. Requests matching %q will NOT be handled. Fix this rule to restore service.",
					check.ID, matchURL)
			m.invalidRules = append(m.invalidRules, check)
		} else {
			m.rules = append(m.rules, check)
		}
	}

	return nil
}

func (m *RepositoryMemory) Match(ctx context.Context, method string, u *url.URL, protocol Protocol) (*Rule, error) {
	if u == nil {
		return nil, errors.WithStack(errors.New("nil URL provided"))
	}

	m.Lock()
	defer m.Unlock()

	var rules []*Rule
	for k := range m.rules {
		r := &m.rules[k]
		if matched, err := r.IsMatching(m.matchingStrategy, method, u, protocol); err != nil {
			return nil, errors.WithStack(err)
		} else if matched {
			rules = append(rules, r)
		}
	}
	for k := range m.invalidRules {
		r := &m.invalidRules[k]
		if matched, err := r.IsMatching(m.matchingStrategy, method, u, protocol); err != nil {
			return nil, errors.WithStack(err)
		} else if matched {
			rules = append(rules, r)
		}
	}

	// ruleInfo is a compact representation used in logs and error messages.
	type ruleInfo struct {
		ID      string
		URL     string
		Methods []string
	}
	toRuleInfo := func(r *Rule) ruleInfo {
		info := ruleInfo{ID: r.ID}
		if r.Match != nil {
			info.URL = r.Match.GetURL()
			info.Methods = r.Match.GetMethods()
		}
		return info
	}
	fmtRule := func(ri ruleInfo) string {
		return fmt.Sprintf("{id=%q url=%q methods=[%s]}", ri.ID, ri.URL, strings.Join(ri.Methods, ","))
	}

	// Snapshot of all currently active rules (valid only).
	activeInfo := make([]ruleInfo, 0, len(m.rules))
	for k := range m.rules {
		activeInfo = append(activeInfo, toRuleInfo(&m.rules[k]))
	}
	activeLines := make([]string, len(activeInfo))
	for i, ri := range activeInfo {
		activeLines[i] = fmtRule(ri)
	}
	activeRulesSummary := strings.Join(activeLines, ", ")

	requestURL := u.String()

	if len(rules) == 0 {
		suggestion := SuggestFix("No Rule Matched", method, requestURL, nil, activeInfo)

		m.r.Logger().
			WithField("request_method", method).
			WithField("request_url", requestURL).
			WithField("active_rules_count", len(activeInfo)).
			WithField("active_rules", activeInfo).
			Errorf("No rule matched %s %s. "+
				"Verify the request URL and method are covered by a rule. "+
				"Active rules (%d): [%s]%s",
				method, requestURL, len(activeInfo), activeRulesSummary, suggestion)

		return nil, errors.WithStack(
			helper.ErrMatchesNoRule.WithReasonf(
				"No rule matched %s %s. "+
					"%d active rule(s) were checked: [%s]. "+
					"Verify the request URL/method and ensure a matching rule is configured.%s",
				method, requestURL, len(activeInfo), activeRulesSummary, suggestion,
			),
		)
	} else if len(rules) != 1 {
		// Collect the conflicting rules.
		conflictingInfo := make([]ruleInfo, 0, len(rules))
		for _, r := range rules {
			conflictingInfo = append(conflictingInfo, toRuleInfo(r))
		}
		conflictLines := make([]string, len(conflictingInfo))
		for i, ri := range conflictingInfo {
			conflictLines[i] = fmtRule(ri)
		}
		conflictSummary := strings.Join(conflictLines, ", ")

		suggestion := SuggestFix("Duplicate Route Detected", method, requestURL, conflictingInfo, activeInfo)

		m.r.Logger().
			WithField("request_method", method).
			WithField("request_url", requestURL).
			WithField("conflicting_rules_count", len(conflictingInfo)).
			WithField("conflicting_rules", conflictingInfo).
			WithField("active_rules_count", len(activeInfo)).
			WithField("active_rules", activeInfo).
			Errorf("Duplicate route detected: %d rules matched %s %s. "+
				"Review the conflicting_rules above and compare against active_rules to locate the overlap. "+
				"Each URL pattern must match at most one rule.%s",
				len(conflictingInfo), method, requestURL, suggestion)

		return nil, errors.WithStack(
			helper.ErrMatchesMoreThanOneRule.WithReasonf(
				"%d rules matched %s %s — each URL pattern must match at most one rule. "+
					"Conflicting rules: [%s]. "+
					"All %d active rule(s): [%s].%s",
				len(conflictingInfo), method, requestURL,
				conflictSummary,
				len(activeInfo), activeRulesSummary, suggestion,
			),
		)
	}

	return rules[0], nil
}

// InvalidRuleCount returns the number of rules that failed validation during Set().
func (m *RepositoryMemory) InvalidRuleCount() int {
	m.RLock()
	defer m.RUnlock()
	return len(m.invalidRules)
}

// InvalidRules returns a copy of the rules that failed validation during Set().
func (m *RepositoryMemory) InvalidRules() []Rule {
	m.RLock()
	defer m.RUnlock()
	out := make([]Rule, len(m.invalidRules))
	copy(out, m.invalidRules)
	return out
}

func (m *RepositoryMemory) ReadyChecker(r *http.Request) error {
	c, err := m.Count(r.Context())
	if err != nil {
		return err
	}
	if c == 0 {
		return errors.WithStack(helper.ErrResourceNotFound.WithReason("No rules found."))
	}
	return nil
}
