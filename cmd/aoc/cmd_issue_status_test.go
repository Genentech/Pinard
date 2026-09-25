package main

import "testing"

func TestIssueStatusCmd_RequiresProjectAndIssue(t *testing.T) {
	if err := issueStatusCmd.RunE(issueStatusCmd, nil); err == nil {
		t.Fatal("expected an error when --project/--issue are unset")
	}
}
