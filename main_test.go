package main

import "testing"

func TestClassifyReplyDecision(t *testing.T) {
	tests := []struct {
		text string
		want ReplyDecision
	}{
		{"YES", DecisionYes},
		{"Yes, that is correct.", DecisionYes},
		{"I agree", DecisionUnknown},
		{"NO", DecisionNo},
		{"No, the amount is wrong", DecisionNo},
		{"That summary needs a correction", DecisionUnknown},
		{"Yesterday was correct", DecisionUnknown},
	}
	for _, test := range tests {
		if got := classifyReplyDecision(test.text); got != test.want {
			t.Errorf("classifyReplyDecision(%q) = %v, want %v", test.text, got, test.want)
		}
	}
}

func TestCanonicalClaimValueAmount(t *testing.T) {
	values := []string{"340", "340.00", "$340.0", "INR 340"}
	for _, value := range values {
		if got := canonicalClaimValue(value, "amount"); got != "340.00" {
			t.Errorf("canonicalClaimValue(%q) = %q", value, got)
		}
	}
}

func TestFindDeterministicContradiction(t *testing.T) {
	tests := []struct {
		name               string
		a, b               []ClaimRecord
		wantRole, wantType string
	}{
		{
			name: "matching normalized amounts",
			a:    []ClaimRecord{{ClaimType: "amount", ValueText: "$340", Confidence: "stated"}},
			b:    []ClaimRecord{{ClaimType: "amount", ValueText: "340.00", Confidence: "stated"}},
		},
		{
			name:     "ask vague party",
			a:        []ClaimRecord{{ClaimType: "amount", ValueText: "340", Confidence: "stated"}},
			b:        []ClaimRecord{{ClaimType: "amount", ValueText: "300", Confidence: "vague"}},
			wantRole: "B", wantType: "amount",
		},
		{
			name:     "stable tie break",
			a:        []ClaimRecord{{ClaimType: "date", ValueText: "March 15", Confidence: "stated"}},
			b:        []ClaimRecord{{ClaimType: "date", ValueText: "March 16", Confidence: "stated"}},
			wantRole: "B", wantType: "date",
		},
		{
			name: "missing counterpart claim is not contradiction",
			a:    []ClaimRecord{{ClaimType: "amount", ValueText: "340", Confidence: "stated"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			role, claimType := findDeterministicContradiction(test.a, test.b)
			if role != test.wantRole || claimType != test.wantType {
				t.Fatalf("got (%q, %q), want (%q, %q)", role, claimType, test.wantRole, test.wantType)
			}
		})
	}
}
