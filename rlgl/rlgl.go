// rlgl.go - Copyright 2018-2022  Anthony Green <green@moxielogic.com>
//
// This program is free software: you can redistribute it and/or
// modify it under the terms of the GNU Affero General Public License
// as published by the Free Software Foundation, either version 3 of
// the License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the GNU
// Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public
// License along with this program.  If not, see
// <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/urfave/cli/v2"
)

var (
	VERSION = "undefined"
	red     = color.New(color.FgRed).SprintFunc()
	green   = color.New(color.FgGreen).SprintFunc()
	cyan    = color.New(color.FgCyan).SprintFunc()
)

func output(s string) {
	fmt.Printf("%s %s\n", cyan("rlgl"), s)
}

func exitErr(err error) {
	output(red(err.Error()))
	os.Exit(2)
}

// --- Test result representation ---

type TestResult struct {
	Fields map[string]string
}

// --- Report parsers ---

// detectFormat checks whether the file is JUnit XML or DejaGnu text.
func detectFormat(filename string) string {
	f, err := os.Open(filename)
	if err != nil {
		return "unknown"
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	content := strings.TrimSpace(string(buf[:n]))
	if strings.HasPrefix(content, "<?xml") || strings.HasPrefix(content, "<test") {
		return "junit"
	}
	return "dejagnu"
}

// parseDejaGnu parses a DejaGnu summary log into test results.
func parseDejaGnu(filename string) ([]TestResult, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var results []TestResult
	host := "UNKNOWN"
	target := "UNKNOWN"

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "Native configuration is "):
			h := strings.TrimPrefix(line, "Native configuration is ")
			host = strings.TrimSpace(h)
			target = host
		case strings.HasPrefix(line, "Host   is"):
			host = strings.TrimSpace(strings.TrimPrefix(line, "Host   is"))
		case strings.HasPrefix(line, "Target is"):
			target = strings.TrimSpace(strings.TrimPrefix(line, "Target is"))
		case strings.HasPrefix(line, "FAIL:"):
			results = append(results, TestResult{Fields: map[string]string{
				"report": "dejagnu", "result": "FAIL",
				"host": host, "target": target,
				"id": strings.TrimSpace(strings.TrimPrefix(line, "FAIL:")),
			}})
		case strings.HasPrefix(line, "XFAIL:"):
			results = append(results, TestResult{Fields: map[string]string{
				"report": "dejagnu", "result": "XFAIL",
				"host": host, "target": target,
				"id": strings.TrimSpace(strings.TrimPrefix(line, "XFAIL:")),
			}})
		case strings.HasPrefix(line, "XPASS:"):
			results = append(results, TestResult{Fields: map[string]string{
				"report": "dejagnu", "result": "XPASS",
				"host": host, "target": target,
				"id": strings.TrimSpace(strings.TrimPrefix(line, "XPASS:")),
			}})
		case strings.HasPrefix(line, "PASS:"):
			results = append(results, TestResult{Fields: map[string]string{
				"report": "dejagnu", "result": "PASS",
				"host": host, "target": target,
				"id": strings.TrimSpace(strings.TrimPrefix(line, "PASS:")),
			}})
		}
	}
	return results, scanner.Err()
}

// JUnit XML structures
type junitTestSuites struct {
	XMLName    xml.Name         `xml:"testsuites"`
	TestSuites []junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Failure   *junitFailure `xml:"failure"`
	Error     *junitError   `xml:"error"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
}

type junitError struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
}

// parseJUnit parses a JUnit XML report into test results.
func parseJUnit(filename string) ([]TestResult, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var results []TestResult

	// Try parsing as <testsuites> wrapper first
	var suites junitTestSuites
	if err := xml.Unmarshal(data, &suites); err == nil && len(suites.TestSuites) > 0 {
		for _, suite := range suites.TestSuites {
			for _, tc := range suite.TestCases {
				results = append(results, junitTestCaseToResult(tc))
			}
		}
		return results, nil
	}

	// Try parsing as a single <testsuite>
	var suite junitTestSuite
	if err := xml.Unmarshal(data, &suite); err == nil {
		for _, tc := range suite.TestCases {
			results = append(results, junitTestCaseToResult(tc))
		}
		return results, nil
	}

	return nil, fmt.Errorf("failed to parse JUnit XML")
}

func junitTestCaseToResult(tc junitTestCase) TestResult {
	result := "PASS"
	if tc.Failure != nil {
		result = "FAIL"
	} else if tc.Error != nil {
		result = "FAIL"
	}
	return TestResult{Fields: map[string]string{
		"report": "junit",
		"result": result,
		"id":     tc.ClassName,
		"name":   tc.Name,
	}}
}

// --- Policy engine ---

type MatchFunc func(value string) bool

type PolicyMatcher struct {
	Kind           string // "XFAIL", "FAIL", "PASS"
	Pattern        map[string]MatchFunc
	ExpirationDate time.Time
}

type Policy struct {
	XFAILMatchers []PolicyMatcher
	FAILMatchers  []PolicyMatcher
	PASSMatchers  []PolicyMatcher
}

var rangeMatcher = regexp.MustCompile(`^\d+(\.\d*)?\.\.(\d+(\.\d*)?)$`)

// compileFieldMatcher creates a MatchFunc for a single pattern value.
// Supports: numeric ranges (N..M), regex (^...), and exact string match.
func compileFieldMatcher(pattern string) MatchFunc {
	if rangeMatcher.MatchString(pattern) {
		parts := strings.SplitN(pattern, "..", 2)
		low, _ := strconv.ParseFloat(parts[0], 64)
		high, _ := strconv.ParseFloat(parts[1], 64)
		return func(value string) bool {
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return false
			}
			return v >= low && v <= high
		}
	}
	if strings.HasPrefix(pattern, "^") {
		re, err := regexp.Compile(pattern + "$")
		if err != nil {
			return func(value string) bool { return false }
		}
		return func(value string) bool {
			return re.MatchString(value)
		}
	}
	return func(value string) bool {
		return value == pattern
	}
}

// extractExpirationDate extracts an optional expiration date after the JSON closing brace.
func extractExpirationDate(line string) time.Time {
	lastBrace := strings.LastIndex(line, "}")
	if lastBrace < 0 || lastBrace >= len(line)-1 {
		return time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	dateStr := strings.TrimSpace(line[lastBrace+1:])
	if dateStr == "" {
		return time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	// Try common date formats
	for _, layout := range []string{
		"2006-01-02 15:04",
		"2006-01-02",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, dateStr); err == nil {
			return t
		}
	}
	return time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
}

// readPolicyFile reads a policy file (XFAIL, FAIL, or PASS) and returns matchers.
func readPolicyFile(filename string, kind string) ([]PolicyMatcher, error) {
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var matchers []PolicyMatcher
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '-' {
			continue
		}

		expDate := extractExpirationDate(line)

		// Extract the JSON object (everything up to and including the last })
		lastBrace := strings.LastIndex(line, "}")
		if lastBrace < 0 {
			continue
		}
		jsonStr := line[:lastBrace+1]

		var patternMap map[string]string
		if err := json.Unmarshal([]byte(jsonStr), &patternMap); err != nil {
			continue
		}

		compiled := make(map[string]MatchFunc)
		for k, v := range patternMap {
			compiled[k] = compileFieldMatcher(v)
		}

		matchers = append(matchers, PolicyMatcher{
			Kind:           kind,
			Pattern:        compiled,
			ExpirationDate: expDate,
		})
	}
	return matchers, scanner.Err()
}

// cloneOrUpdatePolicy clones or updates a git policy repo and returns the local path.
func cloneOrUpdatePolicy(policyURL string) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cacheDir := filepath.Join(homeDir, ".rlgl", "policies")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}

	// Use a hash-like name based on the URL
	safeName := strings.NewReplacer(
		"/", "_", ":", "_", ".", "_",
	).Replace(policyURL)
	policyDir := filepath.Join(cacheDir, safeName)

	if _, err := os.Stat(filepath.Join(policyDir, ".git")); err == nil {
		// Directory exists, pull
		cmd := exec.Command("git", "-C", policyDir, "pull", "--ff-only")
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git pull failed: %s\n%s", err, string(out))
		}
	} else {
		// Clone
		cmd := exec.Command("git", "clone", policyURL, policyDir)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git clone failed: %s\n%s", err, string(out))
		}
	}

	return policyDir, nil
}

// loadPolicy reads the XFAIL, FAIL, and PASS files from a policy directory.
func loadPolicy(policyDir string) (*Policy, error) {
	xfail, err := readPolicyFile(filepath.Join(policyDir, "XFAIL"), "XFAIL")
	if err != nil {
		return nil, fmt.Errorf("reading XFAIL: %w", err)
	}
	fail, err := readPolicyFile(filepath.Join(policyDir, "FAIL"), "FAIL")
	if err != nil {
		return nil, fmt.Errorf("reading FAIL: %w", err)
	}
	pass, err := readPolicyFile(filepath.Join(policyDir, "PASS"), "PASS")
	if err != nil {
		return nil, fmt.Errorf("reading PASS: %w", err)
	}
	return &Policy{
		XFAILMatchers: xfail,
		FAILMatchers:  fail,
		PASSMatchers:  pass,
	}, nil
}

// matchResult checks if a test result matches all fields in a pattern.
func matchResult(result TestResult, matcher PolicyMatcher) bool {
	if time.Now().After(matcher.ExpirationDate) {
		return false
	}
	for field, matchFn := range matcher.Pattern {
		val, ok := result.Fields[field]
		if !ok || !matchFn(val) {
			return false
		}
	}
	return true
}

// applyPolicy evaluates a list of test results against a policy.
// Returns "GREEN" or "RED".
func applyPolicy(policy *Policy, results []TestResult) string {
	colour := "GREEN"

	for _, result := range results {
		matched := false

		// 1. Check XFAIL exceptions (green)
		for _, m := range policy.XFAILMatchers {
			if matchResult(result, m) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		// 2. Check FAIL patterns (red)
		for _, m := range policy.FAILMatchers {
			if matchResult(result, m) {
				colour = "RED"
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		// 3. Check PASS patterns (green)
		for _, m := range policy.PASSMatchers {
			if matchResult(result, m) {
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		// 4. No match = RED
		colour = "RED"
	}

	return colour
}

// loadPolicyFromArg handles both local paths and git URLs.
func loadPolicyFromArg(policyArg string) (*Policy, error) {
	// If it looks like a local directory with policy files, use directly
	if info, err := os.Stat(policyArg); err == nil && info.IsDir() {
		return loadPolicy(policyArg)
	}
	// Otherwise treat as a git URL
	policyDir, err := cloneOrUpdatePolicy(policyArg)
	if err != nil {
		return nil, err
	}
	return loadPolicy(policyDir)
}

// parseReport auto-detects format and parses the report file.
func parseReport(filename string) ([]TestResult, error) {
	format := detectFormat(filename)
	switch format {
	case "junit":
		return parseJUnit(filename)
	case "dejagnu":
		return parseDejaGnu(filename)
	default:
		return nil, fmt.Errorf("unknown report format for %s", filename)
	}
}

func main() {
	var policy string

	app := cli.NewApp()

	app.Commands = []*cli.Command{
		{
			Name:    "evaluate",
			Aliases: []string{"e"},
			Usage:   "evaluate test results against a policy",
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:        "policy",
					Value:       "",
					Usage:       "policy git repo URL or local directory path",
					Destination: &policy,
				},
			},

			Action: func(c *cli.Context) error {
				if policy == "" {
					exitErr(fmt.Errorf("missing --policy"))
				}
				if c.NArg() == 0 {
					exitErr(fmt.Errorf("missing report file argument"))
				}

				reportFile := c.Args().Get(0)

				// Parse the report
				results, err := parseReport(reportFile)
				if err != nil {
					exitErr(fmt.Errorf("parsing report: %s", err))
				}

				if len(results) == 0 {
					output("No test results found in report")
					os.Exit(2)
				}

				// Load the policy
				pol, err := loadPolicyFromArg(policy)
				if err != nil {
					exitErr(fmt.Errorf("loading policy: %s", err))
				}

				// Evaluate
				colour := applyPolicy(pol, results)

				output(fmt.Sprintf("Evaluated %d test results", len(results)))

				if colour == "GREEN" {
					output(green("GREEN"))
					os.Exit(0)
				} else {
					output(red("RED"))
					os.Exit(1)
				}
				return nil
			},
		},
	}

	app.Name = "rlgl"
	app.Version = VERSION
	app.Copyright = "(c) 2018-2022 Anthony Green"
	app.Compiled = time.Now()
	app.Authors = []*cli.Author{
		{
			Name:  "Anthony Green",
			Email: "green@moxielogic.com",
		},
	}
	app.Usage = "Red Light Green Light - Local Evaluation"
	app.Action = func(c *cli.Context) error {
		return cli.ShowAppHelp(c)
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
