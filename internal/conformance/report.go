/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package conformance

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// jsonReport renders results as indented JSON (results.json).
func jsonReport(results *Results) ([]byte, error) {
	return json.MarshalIndent(results, "", "  ")
}

// junitSuite is the JUnit XML testsuite element.
type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

// junitCase is one JUnit testcase: a conformance test.
type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitMessage `xml:"failure,omitempty"`
	Skipped   *junitMessage `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

// junitMessage is a failure or skipped element.
type junitMessage struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// seconds renders d in seconds for JUnit.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%.2f", d.Seconds())
}

// stepLines renders a test's step and cleanup results, one per line.
func stepLines(t TestResult) string {
	var b strings.Builder
	write := func(prefix string, s StepResult) {
		fmt.Fprintf(&b, "%s%s: %s (%v)", prefix, s.Name, s.Status, s.Duration.Round(time.Millisecond))
		if s.Error != "" {
			fmt.Fprintf(&b, ": %s", s.Error)
		}
		b.WriteString("\n")
	}
	for _, s := range t.Steps {
		write("step ", s)
	}
	for _, s := range t.Cleanup {
		write("cleanup ", s)
	}
	for _, k := range t.Kept {
		fmt.Fprintf(&b, "kept %s\n", k)
	}
	return b.String()
}

// junitReport renders results as JUnit XML (junit.xml): one testcase per
// test, with the step results in system-out.
func junitReport(results *Results) ([]byte, error) {
	suite := junitSuite{
		Name:     "virtrigaud-conformance",
		Tests:    results.Total,
		Failures: results.Failed,
		Skipped:  results.Skipped,
		Time:     seconds(results.Duration),
	}
	for _, t := range results.Tests {
		c := junitCase{
			Name:      t.Name,
			ClassName: "virtrigaud-conformance." + results.Provider,
			Time:      seconds(t.Duration),
			SystemOut: stepLines(t),
		}
		switch t.Status {
		case StatusFailed:
			c.Failure = &junitMessage{Message: t.Error, Text: t.Error}
		case StatusSkipped:
			c.Skipped = &junitMessage{Message: t.SkipReason}
		}
		suite.Cases = append(suite.Cases, c)
	}
	out, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// mdCell escapes a value for a Markdown table cell.
func mdCell(s string) string {
	if s == "" {
		return "-"
	}
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

// markdownReport renders results as Markdown (report.md): a summary, a
// table of tests and, per test, a table of its steps.
func markdownReport(results *Results) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# VirtRigaud Conformance Test Report

## Summary

- **Provider**: %s
- **Namespace**: %s
- **Run ID**: %s
- **Total Tests**: %d
- **Passed**: %d
- **Failed**: %d
- **Skipped**: %d
- **Duration**: %v
- **Timestamp**: %s

## Test Results

| Test Name | Status | Duration | Error or skip reason |
|-----------|--------|----------|----------------------|
`, results.Provider, results.Namespace, results.RunID, results.Total, results.Passed, results.Failed,
		results.Skipped, results.Duration.Round(time.Millisecond), results.Timestamp.Format(time.RFC3339))

	for _, t := range results.Tests {
		detail := t.Error
		if t.Status == StatusSkipped {
			detail = t.SkipReason
		}
		fmt.Fprintf(&b, "| %s | %s | %v | %s |\n", mdCell(t.Name), t.Status, t.Duration.Round(time.Millisecond), mdCell(detail))
	}

	for _, t := range results.Tests {
		if len(t.Steps) == 0 && len(t.Cleanup) == 0 && len(t.Kept) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Step | Status | Duration | Error |\n|------|--------|----------|-------|\n", t.Name)
		for _, s := range t.Steps {
			fmt.Fprintf(&b, "| %s | %s | %v | %s |\n", mdCell(s.Name), s.Status, s.Duration.Round(time.Millisecond), mdCell(s.Error))
		}
		for _, s := range t.Cleanup {
			fmt.Fprintf(&b, "| cleanup: %s | %s | %v | %s |\n", mdCell(s.Name), s.Status, s.Duration.Round(time.Millisecond), mdCell(s.Error))
		}
		if len(t.Kept) > 0 {
			fmt.Fprintf(&b, "\nKept (--keep): %s\n", strings.Join(t.Kept, ", "))
		}
	}
	return b.String()
}
