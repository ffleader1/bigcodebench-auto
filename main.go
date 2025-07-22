package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// sendNotification sends a cross-platform notification
func sendNotification(title, message string) {
	system := runtime.GOOS

	switch system {
	case "windows":
		// Use PowerShell for Windows toast notification
		psCmd := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; `+
			`$notification = New-Object System.Windows.Forms.NotifyIcon; `+
			`$notification.Icon = [System.Drawing.SystemIcons]::Information; `+
			`$notification.BalloonTipTitle = '%s'; `+
			`$notification.BalloonTipText = '%s'; `+
			`$notification.Visible = $true; `+
			`$notification.ShowBalloonTip(3000)`, title, message)

		cmd := exec.Command("powershell", "-Command", psCmd)
		cmd.Run() // Ignore errors for notifications

	case "darwin": // macOS
		applescript := fmt.Sprintf(`display notification "%s" with title "%s"`, message, title)
		cmd := exec.Command("osascript", "-e", applescript)
		cmd.Run() // Ignore errors for notifications

	case "linux":
		// Try notify-send first
		cmd := exec.Command("notify-send", title, message)
		if cmd.Run() != nil {
			// Fallback to zenity
			cmd = exec.Command("zenity", "--info", "--title="+title, "--text="+message)
			if cmd.Run() != nil {
				// Final fallback to console
				fmt.Printf("NOTIFICATION: %s - %s\n", title, message)
			}
		}

	default:
		fmt.Printf("NOTIFICATION: %s - %s\n", title, message)
	}
}

// readEnvFile reads the environment file to get TASK_ID
func readEnvFile(envPath string) (string, error) {
	file, err := os.Open(envPath)
	if err != nil {
		return "", fmt.Errorf("environment file '%s' not found: %w", envPath, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "TASK_ID=") {
			taskID := strings.SplitN(line, "=", 2)[1]
			return taskID, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading environment file: %w", err)
	}

	return "", fmt.Errorf("TASK_ID not found in environment file")
}

// calculateFileHash calculates SHA256 hash of a file
func calculateFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// generateCombinedHash generates hash from response file and test file
func generateCombinedHash(responseFile, testFile string) (string, error) {
	responseHash, err := calculateFileHash(responseFile)
	if err != nil {
		return "", fmt.Errorf("error hashing response file: %w", err)
	}

	testHash, err := calculateFileHash(testFile)
	if err != nil {
		return "", fmt.Errorf("error hashing test file: %w", err)
	}

	combinedString := responseHash + testHash
	hasher := sha256.New()
	hasher.Write([]byte(combinedString))
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// readHashCache reads the hash from cache file
func readHashCache(cacheFile string) (string, error) {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeHashCache writes the hash to cache file
func writeHashCache(cacheFile, hash string) error {
	return os.WriteFile(cacheFile, []byte(hash), 0644)
}

// getPackageName extracts package name from a Go file
func getPackageName(goFilePath string) (string, error) {
	content, err := os.ReadFile(goFilePath)
	if err != nil {
		return "", fmt.Errorf("error reading %s: %w", goFilePath, err)
	}

	re := regexp.MustCompile(`(?m)^package\s+(\w+)`)
	matches := re.FindSubmatch(content)
	if len(matches) > 1 {
		return string(matches[1]), nil
	}

	return "", fmt.Errorf("package declaration not found")
}

// modifyPackageToMain copies Go file and changes package to main if necessary
func modifyPackageToMain(goFilePath, tempFilePath string) error {
	content, err := os.ReadFile(goFilePath)
	if err != nil {
		return fmt.Errorf("error reading %s: %w", goFilePath, err)
	}

	// Replace package declaration with 'package main'
	re := regexp.MustCompile(`(?m)^package\s+\w+`)
	modifiedContent := re.ReplaceAll(content, []byte("package main"))

	err = os.WriteFile(tempFilePath, modifiedContent, 0644)
	if err != nil {
		return fmt.Errorf("error writing modified file: %w", err)
	}

	return nil
}

// TestResult holds the result of a test run
type TestResult struct {
	Name           string
	Success        bool
	Output         string
	LineCoverage   float64
	BranchCoverage float64
	CoverageReport string
	Cached         bool
}

// runMainCoverageAnalysis runs coverage analysis on main.go if it exists
func runMainCoverageAnalysis(taskDir string) (float64, float64, string) {
	mainGoFile := filepath.Join(taskDir, "main.go")
	//testFile := filepath.Join(taskDir, "main_test.go")

	// Check if both main.go and main_test.go exist
	if _, err := os.Stat(mainGoFile); os.IsNotExist(err) {
		return 0.0, 0.0, "main.go not found - skipping coverage analysis"
	}

	var coverageOutput strings.Builder
	var lineCoverage, branchCoverage float64

	// Cleanup coverage files
	defer func() {
		coverageFiles := []string{"coverage.out", "coverage.html"}
		for _, file := range coverageFiles {
			if _, err := os.Stat(filepath.Join(taskDir, file)); err == nil {
				os.Remove(filepath.Join(taskDir, file))
			}
		}
	}()

	coverageOutput.WriteString("=== Coverage Analysis for main.go ===\n\n")

	// Run line coverage analysis
	coverageCmd := exec.Command("go", "test", "-coverprofile=coverage.out")
	coverageCmd.Dir = taskDir

	if cmdOutput, err := coverageCmd.CombinedOutput(); err == nil {
		coverageOutput.WriteString("=== Line Coverage Analysis ===\n")
		coverageOutput.WriteString(string(cmdOutput))
		lineCoverage = parseCoverageReport(string(cmdOutput))

	} else {
		coverageOutput.WriteString(fmt.Sprintf("Line coverage analysis failed: %v\n%s\n", err, string(cmdOutput)))
	}

	// Run branch coverage analysis with gobco
	gobcoCmd := exec.Command("gobco")
	gobcoCmd.Dir = taskDir

	if gobcoOutput, err := gobcoCmd.CombinedOutput(); err == nil {
		coverageOutput.WriteString("\n=== Branch Coverage Analysis (gobco) ===\n")
		coverageOutput.WriteString(string(gobcoOutput))

		branchCov, coverageReport := parseGobcoCoverage(string(gobcoOutput))

		branchCoverage = branchCov
		if coverageReport != "" {
			coverageOutput.WriteString("\n")
			coverageOutput.WriteString(coverageReport)
		}
	} else {
		coverageOutput.WriteString(fmt.Sprintf("\nBranch coverage analysis failed (gobco): %v\n", err))
		coverageOutput.WriteString("Note: Make sure 'gobco' is installed: go install github.com/rillig/gobco@latest\n")
	}

	return lineCoverage, branchCoverage, coverageOutput.String()
}

func parseCoverageReport(output string) float64 {
	// Look for coverage percentage in the output
	re := regexp.MustCompile(`coverage:\s+(\d+\.?\d*)%`)
	matches := re.FindStringSubmatch(output)
	if len(matches) > 1 {
		if coverage, err := strconv.ParseFloat(matches[1], 64); err == nil {
			return coverage
		}
	}
	return 0.0
}

// parseGobcoCoverage parses gobco output to extract branch coverage
func parseGobcoCoverage(output string) (float64, string) {
	lines := strings.Split(output, "\n")
	var coverageReport strings.Builder
	var branchCoverage float64

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Look for "Condition coverage: X/Y" pattern
		if strings.Contains(line, "Condition coverage:") {
			re := regexp.MustCompile(`Condition coverage:\s+(\d+)/(\d+)`)
			matches := re.FindStringSubmatch(line)
			if len(matches) >= 3 {
				covered, _ := strconv.ParseFloat(matches[1], 64)
				total, _ := strconv.ParseFloat(matches[2], 64)
				if total > 0 {
					branchCoverage = (covered / total) * 100
				}
				coverageReport.WriteString(fmt.Sprintf("Branch Coverage: %.1f%% (%s)\n", branchCoverage, strings.TrimPrefix(line, "Condition coverage: ")))
			}
		}

		// Include condition details
		if strings.Contains(line, "condition") && (strings.Contains(line, "never") || strings.Contains(line, "times")) {
			coverageReport.WriteString(line + "\n")
		}
	}

	return branchCoverage, coverageReport.String()
}

// loadCachedResult loads cached result from result.txt file
func loadCachedResult(responseFolder string) (TestResult, error) {
	resultFile := filepath.Join(responseFolder, "result.txt")
	data, err := os.ReadFile(resultFile)
	if err != nil {
		return TestResult{}, err
	}

	responseName := filepath.Base(responseFolder)
	output := string(data)

	// Simple check if test passed based on output content
	success := strings.Contains(output, "PASSED") || strings.Contains(output, "ok\t")

	return TestResult{
		Name:    responseName,
		Success: success,
		Output:  output,
		Cached:  true,
	}, nil
}

// runGoTest runs go test for a specific response file
func runGoTest(responseFile, testFile, workDir string) TestResult {
	responseFolder := filepath.Dir(responseFile)
	responseName := filepath.Base(responseFile)
	responseName = strings.TrimSuffix(responseName, ".go")

	cacheFile := filepath.Join(responseFolder, "hash.cache")

	// Generate current hash
	currentHash, err := generateCombinedHash(responseFile, testFile)
	if err != nil {
		return TestResult{
			Name:    responseName,
			Success: false,
			Output:  fmt.Sprintf("Failed to generate hash: %v", err),
		}
	}

	// Check if hash matches cached version
	if cachedHash, err := readHashCache(cacheFile); err == nil && cachedHash == currentHash {
		// Hash matches, try to load cached result
		if cachedResult, err := loadCachedResult(responseFolder); err == nil {
			fmt.Printf("🚀 %s - Using cached result (hash match)\n", responseName)
			return cachedResult
		}
	}

	tempResponse := filepath.Join(workDir, "temp_"+filepath.Base(responseFile))

	result := TestResult{
		Name:           responseName,
		Success:        false,
		Output:         "",
		LineCoverage:   0.0,
		BranchCoverage: 0.0,
		CoverageReport: "",
		Cached:         false,
	}

	// Copy and modify the response file
	err = modifyPackageToMain(responseFile, tempResponse)
	if err != nil {
		result.Output = fmt.Sprintf("Failed to modify package: %v", err)
		return result
	}

	// Ensure cleanup
	defer func() {
		if _, err := os.Stat(tempResponse); err == nil {
			os.Remove(tempResponse)
		}
		// Clean up coverage files
		coverageFiles := []string{"coverage.out", "coverage.html"}
		for _, file := range coverageFiles {
			if _, err := os.Stat(filepath.Join(workDir, file)); err == nil {
				os.Remove(filepath.Join(workDir, file))
			}
		}
	}()

	var testOutput strings.Builder

	// Run basic test first
	cmd := exec.Command("go", "test", "-v",
		filepath.Base(testFile),
		"temp_"+filepath.Base(responseFile))
	cmd.Dir = workDir

	done := make(chan error, 1)
	go func() {
		output, err := cmd.CombinedOutput()
		testOutput.WriteString("=== Basic Test Output ===\n")
		testOutput.WriteString(string(output))
		testOutput.WriteString("\n")

		if err != nil {
			testOutput.WriteString(fmt.Sprintf("Test Error: %v\n", err))
			done <- err
		} else {
			result.Success = true
			done <- nil
		}
	}()

	select {
	case _ = <-done:
		// Test result handled above
	case <-time.After(30 * time.Second): // Back to original timeout
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		testOutput.WriteString("Test timed out after 30 seconds")
	}

	result.Output = testOutput.String()

	// Write individual result.txt file
	resultFile := filepath.Join(responseFolder, "result.txt")
	if err := os.WriteFile(resultFile, []byte(result.Output), 0644); err != nil {
		fmt.Printf("Warning: Could not write result file for %s: %v\n", responseName, err)
	}

	// Update hash cache
	if err := writeHashCache(cacheFile, currentHash); err != nil {
		fmt.Printf("Warning: Could not write hash cache for %s: %v\n", responseName, err)
	}

	return result
}

// writeResults writes all test results to a file
func writeResults(resultsFile string, taskID string, results []TestResult, mainLineCoverage, mainBranchCoverage float64, mainCoverageReport string) error {
	file, err := os.Create(resultsFile)
	if err != nil {
		return fmt.Errorf("error creating results file: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	passedCount := 0
	cachedCount := 0
	for _, result := range results {
		if result.Success {
			passedCount++
		}
		if result.Cached {
			cachedCount++
		}
	}

	fmt.Fprintf(writer, "Go Test Results for Task %s\n", taskID)
	fmt.Fprintf(writer, "%s\n\n", strings.Repeat("=", 60))
	fmt.Fprintf(writer, "Summary: %d/%d tests passed (%d cached)\n\n", passedCount, len(results), cachedCount)

	// Add main.go coverage analysis if available
	if mainLineCoverage > 0 || mainBranchCoverage > 0 || mainCoverageReport != "" {
		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 60))
		fmt.Fprintf(writer, "MAIN.GO COVERAGE ANALYSIS\n")
		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 60))
		if mainLineCoverage > 0 || mainBranchCoverage > 0 {
			fmt.Fprintf(writer, "Line Coverage: %.1f%%\n", mainLineCoverage)
			fmt.Fprintf(writer, "Branch Coverage: %.1f%%\n", mainBranchCoverage)
		}
		fmt.Fprintf(writer, "\n%s\n\n", mainCoverageReport)
	}

	for _, result := range results {
		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 40))
		fmt.Fprintf(writer, "Test: %s", result.Name)
		if result.Cached {
			fmt.Fprintf(writer, " (CACHED)")
		}
		fmt.Fprintf(writer, "\n")
		if result.Success {
			fmt.Fprintf(writer, "Status: PASSED\n")
		} else {
			fmt.Fprintf(writer, "Status: FAILED\n")
		}

		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 40))
		fmt.Fprintf(writer, "Output:\n%s\n\n", result.Output)
	}

	return nil
}

func main() {
	// Read environment file
	taskID, err := readEnvFile("env")
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// Check if task directory exists
	taskDir := taskID
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		fmt.Printf("Error: Task directory '%s' not found!\n", taskID)
		os.Exit(1)
	}

	fmt.Printf("Working with task ID: %s\n", taskID)
	absPath, _ := filepath.Abs(taskDir)
	fmt.Printf("Task directory: %s\n", absPath)

	// Find main_test.go
	testFile := filepath.Join(taskDir, "main_test.go")
	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		fmt.Printf("Error: main_test.go not found in %s\n", taskDir)
		os.Exit(1)
	}

	// Find all response folders
	var responsePairs [][]string
	for i := 1; i <= 9; i++ {
		responseFolder := filepath.Join(taskDir, "response"+strconv.Itoa(i))
		responseFile := filepath.Join(responseFolder, "response"+strconv.Itoa(i)+".go")

		if _, err := os.Stat(responseFolder); err == nil {
			if _, err := os.Stat(responseFile); err == nil {
				responsePairs = append(responsePairs, []string{responseFolder, responseFile})
			}
		}
	}

	if len(responsePairs) == 0 {
		fmt.Println("Error: No response folders with .go files found!")
		os.Exit(1)
	}

	fmt.Printf("Found %d response files to test\n", len(responsePairs))

	// Results tracking
	var results []TestResult
	passedCount := 0
	cachedCount := 0

	// Run coverage analysis on main.go if it exists (separate from individual response tests)
	mainLineCoverage, mainBranchCoverage, mainCoverageReport := runMainCoverageAnalysis(taskDir)

	fmt.Printf("Main line coverage: %.1f%%\n", mainLineCoverage)
	fmt.Printf("Main branch coverage: %.1f%%\n", mainBranchCoverage)

	// Test each response
	for _, pair := range responsePairs {
		responseFile := pair[1]
		responseName := filepath.Base(responseFile)
		responseName = strings.TrimSuffix(responseName, ".go")

		fmt.Printf("\n%s\n", strings.Repeat("=", 50))
		fmt.Printf("Testing %s...\n", responseName)
		fmt.Printf("%s\n", strings.Repeat("=", 50))

		result := runGoTest(responseFile, testFile, taskDir)
		results = append(results, result)

		if result.Success {
			passedCount++
			if result.Cached {
				cachedCount++
				fmt.Printf("✅ %s PASSED! (cached)\n", responseName)
			} else {
				fmt.Printf("✅ %s PASSED!\n", responseName)
				sendNotification("Go Test Passed! 🎉", fmt.Sprintf("%s passed all tests!", responseName))
			}
		} else {
			fmt.Printf("❌ %s FAILED\n", responseName)
		}

		//if !result.Cached {
		//	fmt.Printf("Output:\n%s\n", result.Output)
		//}
	}

	// Write results to file
	resultsFile := filepath.Join(taskDir, "result.txt")
	if err := writeResults(resultsFile, taskID, results, mainLineCoverage, mainBranchCoverage, mainCoverageReport); err != nil {
		fmt.Printf("Error writing results: %v\n", err)
	}

	fmt.Printf("\n%s\n", strings.Repeat("=", 60))
	fmt.Printf("FINAL RESULTS\n")
	fmt.Printf("%s\n", strings.Repeat("=", 60))
	fmt.Printf("Total tests: %d\n", len(results))
	fmt.Printf("Passed: %d\n", passedCount)
	fmt.Printf("Failed: %d\n", len(results)-passedCount)
	if cachedCount > 0 {
		fmt.Printf("Cached: %d\n", cachedCount)
	}

	// Display main.go coverage if available
	if mainLineCoverage > 0 || mainBranchCoverage > 0 {
		fmt.Printf("\nMAIN.GO COVERAGE:\n")
		fmt.Printf("Line Coverage: %.1f%%\n", mainLineCoverage)
		fmt.Printf("Branch Coverage: %.1f%%\n", mainBranchCoverage)
	}

	absResultsPath, _ := filepath.Abs(resultsFile)
	fmt.Printf("\nResults written to: %s\n", absResultsPath)

	if passedCount > 0 {
		sendNotification(
			"Testing Complete! 🏆",
			fmt.Sprintf("%d/%d tests passed for Task %s", passedCount, len(results), taskID),
		)
	}
}
