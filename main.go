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

// takeScreenshot takes a screenshot and saves it to pictures folder only
func takeScreenshot(screenshotName, taskDir string) error {
	// Create pictures folder if it doesn't exist
	picturesFolder := filepath.Join(taskDir, "pictures")
	if err := os.MkdirAll(picturesFolder, 0755); err != nil {
		return fmt.Errorf("failed to create pictures folder: %w", err)
	}

	screenshotPath := filepath.Join(picturesFolder, screenshotName)

	system := runtime.GOOS
	var cmd *exec.Cmd

	switch system {
	case "windows":
		// Use PowerShell to take DPI-aware full screen screenshot on Windows
		psScript := fmt.Sprintf(`
		Add-Type -AssemblyName System.Windows.Forms
		Add-Type -AssemblyName System.Drawing
		Add-Type @"
			using System;
			using System.Runtime.InteropServices;
			public class DPI {
				[DllImport("user32.dll")]
				public static extern bool SetProcessDPIAware();
				[DllImport("user32.dll")]
				public static extern int GetSystemMetrics(int nIndex);
			}
"@
		[DPI]::SetProcessDPIAware()
		$screenWidth = [DPI]::GetSystemMetrics(0)
		$screenHeight = [DPI]::GetSystemMetrics(1)
		$bitmap = New-Object System.Drawing.Bitmap $screenWidth, $screenHeight
		$graphics = [System.Drawing.Graphics]::FromImage($bitmap)
		$graphics.CopyFromScreen(0, 0, 0, 0, [System.Drawing.Size]::new($screenWidth, $screenHeight))
		$bitmap.Save('%s')
		$graphics.Dispose()
		$bitmap.Dispose()
	`, screenshotPath)

		cmd = exec.Command("powershell", "-Command", psScript)

	case "darwin": // macOS
		cmd = exec.Command("screencapture", "-x", screenshotPath)

	case "linux":
		// Try different screenshot tools with full screen options
		tools := [][]string{
			{"gnome-screenshot", "-f", screenshotPath},
			{"scrot", screenshotPath},
			{"import", "-window", "root", screenshotPath},
			{"maim", screenshotPath},
		}

		var err error
		for _, tool := range tools {
			cmd = exec.Command(tool[0], tool[1:]...)
			if err = cmd.Run(); err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("no screenshot tool available on Linux")
		}
		return nil

	default:
		return fmt.Errorf("screenshot not supported on %s", system)
	}

	return cmd.Run()
}

// openTerminalAndRunTest opens a terminal and runs the test
func openTerminalAndRunTest(responseFile, testFile, workDir, responseName string) (TestResult, error) {
	system := runtime.GOOS

	tempResponse := filepath.Join(workDir, "temp_"+filepath.Base(responseFile))
	signalFile := filepath.Join(workDir, fmt.Sprintf("screenshot_done_%s.signal", responseName))

	result := TestResult{
		Name:           responseName,
		Success:        false,
		Output:         "",
		LineCoverage:   0.0,
		BranchCoverage: 0.0,
		CoverageReport: "",
		Cached:         false,
	}

	// Clean up any leftover signal files at the start
	if _, err := os.Stat(signalFile); err == nil {
		os.Remove(signalFile)
		fmt.Printf("🧹 Cleaned up leftover signal file: %s\n", signalFile)
	}

	// Copy and modify the response file
	err := modifyPackageToMain(responseFile, tempResponse)
	if err != nil {
		result.Output = fmt.Sprintf("Failed to modify package: %v", err)
		return result, err
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
		// Clean up signal file
		if _, err := os.Stat(signalFile); err == nil {
			os.Remove(signalFile)
		}
	}()

	// Prepare the test command
	testCmd := fmt.Sprintf("go test -v %s %s",
		filepath.Base(testFile),
		"temp_"+filepath.Base(responseFile))

	var cmd *exec.Cmd

	switch system {
	case "windows":
		// Create a batch file that waits for screenshot signal
		batchContent := fmt.Sprintf(`@echo off
cd /d "%s"
echo Testing %s...
echo.
%s
echo.
echo Test completed. Waiting for screenshot...
:wait
if exist "screenshot_done_%s.signal" (
    del "screenshot_done_%s.signal" 2>nul
    timeout /t 4 /nobreak > nul
    echo Screenshot processing complete. Closing window...
    timeout /t 1 /nobreak > nul
    exit
) else (
    timeout /t 1 /nobreak > nul
    goto wait
)
`, workDir, responseName, testCmd, responseName, responseName)

		batchFile := filepath.Join(workDir, fmt.Sprintf("test_%s.bat", responseName))
		if err := os.WriteFile(batchFile, []byte(batchContent), 0644); err != nil {
			return result, err
		}
		defer os.Remove(batchFile)

		cmd = exec.Command("cmd", "/c", "start", "cmd", "/c", batchFile)

	case "darwin": // macOS
		// Create an AppleScript that waits for signal file
		script := fmt.Sprintf(`
tell application "Terminal"
	activate
	set newTab to do script "cd '%s' && echo 'Testing %s...' && echo '' && %s && echo '' && echo 'Test completed. Waiting for screenshot...' && while [ ! -f 'screenshot_done_%s.signal' ]; do sleep 1; done && echo 'Screenshot taken. Closing window...' && rm -f 'screenshot_done_%s.signal' && sleep 1 && exit"
end tell
`, workDir, responseName, testCmd, responseName, responseName)

		cmd = exec.Command("osascript", "-e", script)

	case "linux":
		// Create a script that waits for signal file
		waitScript := fmt.Sprintf("cd '%s' && echo 'Testing %s...' && echo '' && %s && echo '' && echo 'Test completed. Waiting for screenshot...' && while [ ! -f 'screenshot_done_%s.signal' ]; do sleep 1; done && echo 'Screenshot taken. Closing window...' && rm -f 'screenshot_done_%s.signal' && sleep 1 && exit", workDir, responseName, testCmd, responseName, responseName)

		terminals := [][]string{
			{"gnome-terminal", "--", "bash", "-c", waitScript},
			{"xterm", "-e", fmt.Sprintf("bash -c \"%s\"", waitScript)},
			{"konsole", "-e", fmt.Sprintf("bash -c \"%s\"", waitScript)},
		}

		var terminalErr error
		for _, terminal := range terminals {
			cmd = exec.Command(terminal[0], terminal[1:]...)
			if _, terminalErr = exec.LookPath(terminal[0]); terminalErr == nil {
				break
			}
		}
		if terminalErr != nil {
			return result, fmt.Errorf("no suitable terminal emulator found")
		}

	default:
		return result, fmt.Errorf("unsupported operating system: %s", system)
	}

	// Start the terminal
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("failed to start terminal: %w", err)
	}

	// Run test separately to get result and timing
	testCmdExec := exec.Command("go", "test", "-v",
		filepath.Base(testFile),
		"temp_"+filepath.Base(responseFile))
	testCmdExec.Dir = workDir

	startTime := time.Now()
	output, testErr := testCmdExec.CombinedOutput()
	testDuration := time.Since(startTime)

	// Wait for test to complete and display (test time + small buffer for display)
	displayWait := testDuration + (1 * time.Second)
	time.Sleep(displayWait)

	// Take screenshot while terminal is still open
	fmt.Printf("📸 Taking screenshot for %s...\n", responseName)
	screenshotName := fmt.Sprintf("%s.png", responseName)
	if err := takeScreenshot(screenshotName, workDir); err != nil {
		fmt.Printf("Warning: Could not take screenshot for %s: %v\n", responseName, err)
	} else {
		fmt.Printf("✅ Screenshot saved for %s\n", responseName)
	}

	// Signal terminal to close by creating the signal file
	if err := os.WriteFile(signalFile, []byte("done"), 0644); err != nil {
		fmt.Printf("Warning: Could not create signal file for %s: %v\n", responseName, err)
	}

	// Wait a moment for terminal to process the signal and close gracefully
	time.Sleep(2 * time.Second)

	result.Output = fmt.Sprintf("=== Test Output ===\n%s\n", string(output))
	if testErr == nil {
		result.Success = true
	} else {
		result.Output += fmt.Sprintf("\nTest Error: %v\n", testErr)
	}

	return result, nil
}

// openTerminalAndRunMainTest opens a terminal and runs coverage analysis for main.go
func openTerminalAndRunMainTest(taskDir, testType string) (string, error) {
	system := runtime.GOOS
	signalFile := filepath.Join(taskDir, fmt.Sprintf("screenshot_done_%s.signal", testType))

	// Clean up any leftover signal files at the start
	if _, err := os.Stat(signalFile); err == nil {
		os.Remove(signalFile)
		fmt.Printf("🧹 Cleaned up leftover signal file: %s\n", signalFile)
	}

	var testCmd string
	if testType == "line_coverage" {
		testCmd = "go test -coverprofile=coverage.out && go tool cover -html=coverage.out -o coverage.html"
	} else { // branch_coverage
		testCmd = "gobco"
	}

	var cmd *exec.Cmd

	switch system {
	case "windows":
		// Create a batch file that waits for screenshot signal
		batchContent := fmt.Sprintf(`@echo off
cd /d "%s"
echo Running %s analysis for main.go...
echo.
%s
echo.
echo Analysis completed. Waiting for screenshot...
:wait
if exist "screenshot_done_%s.signal" (
    del "screenshot_done_%s.signal" 2>nul
    timeout /t 4 /nobreak > nul
    echo Screenshot processing complete. Closing window...
    timeout /t 1 /nobreak > nul
    exit
) else (
    timeout /t 1 /nobreak > nul
    goto wait
)
`, taskDir, testType, testCmd, testType, testType)

		batchFile := filepath.Join(taskDir, fmt.Sprintf("main_%s.bat", testType))
		if err := os.WriteFile(batchFile, []byte(batchContent), 0644); err != nil {
			return "", err
		}
		defer os.Remove(batchFile)

		cmd = exec.Command("cmd", "/c", "start", "cmd", "/c", batchFile)

	case "darwin": // macOS
		// Create an AppleScript that waits for signal file
		script := fmt.Sprintf(`
tell application "Terminal"
	activate
	set newTab to do script "cd '%s' && echo 'Running %s analysis for main.go...' && echo '' && %s && echo '' && echo 'Analysis completed. Waiting for screenshot...' && while [ ! -f 'screenshot_done_%s.signal' ]; do sleep 1; done && echo 'Screenshot taken. Closing window...' && rm -f 'screenshot_done_%s.signal' && sleep 1 && exit"
end tell
`, taskDir, testType, testCmd, testType, testType)

		cmd = exec.Command("osascript", "-e", script)

	case "linux":
		// Create a script that waits for signal file
		waitScript := fmt.Sprintf("cd '%s' && echo 'Running %s analysis for main.go...' && echo '' && %s && echo '' && echo 'Analysis completed. Waiting for screenshot...' && while [ ! -f 'screenshot_done_%s.signal' ]; do sleep 1; done && echo 'Screenshot taken. Closing window...' && rm -f 'screenshot_done_%s.signal' && sleep 1 && exit", taskDir, testType, testCmd, testType, testType)

		terminals := [][]string{
			{"gnome-terminal", "--", "bash", "-c", waitScript},
			{"xterm", "-e", fmt.Sprintf("bash -c \"%s\"", waitScript)},
			{"konsole", "-e", fmt.Sprintf("bash -c \"%s\"", waitScript)},
		}

		var terminalErr error
		for _, terminal := range terminals {
			cmd = exec.Command(terminal[0], terminal[1:]...)
			if _, terminalErr = exec.LookPath(terminal[0]); terminalErr == nil {
				break
			}
		}
		if terminalErr != nil {
			return "", fmt.Errorf("no suitable terminal emulator found")
		}

	default:
		return "", fmt.Errorf("unsupported operating system: %s", system)
	}

	// Start the terminal
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start terminal: %w", err)
	}

	// Run the actual command to get output
	var actualCmd *exec.Cmd
	if testType == "line_coverage" {
		actualCmd = exec.Command("go", "test", "-coverprofile=coverage.out")
	} else {
		actualCmd = exec.Command("gobco")
	}
	actualCmd.Dir = taskDir

	startTime := time.Now()
	output, err := actualCmd.CombinedOutput()
	testDuration := time.Since(startTime)

	// Wait for command to complete and display
	displayWait := testDuration + (1 * time.Second)
	time.Sleep(displayWait)

	// Take screenshot while terminal is still open
	var screenshotName string
	if testType == "line_coverage" {
		screenshotName = "ideal_line_coverage.png"
	} else {
		screenshotName = "ideal_branch_coverage.png"
	}

	fmt.Printf("📸 Taking screenshot for %s...\n", testType)
	if screenshotErr := takeScreenshot(screenshotName, taskDir); screenshotErr != nil {
		fmt.Printf("Warning: Could not take screenshot for %s: %v\n", testType, screenshotErr)
	} else {
		fmt.Printf("✅ Screenshot saved for %s\n", testType)
	}

	// Signal terminal to close by creating the signal file
	if err := os.WriteFile(signalFile, []byte("done"), 0644); err != nil {
		fmt.Printf("Warning: Could not create signal file for %s: %v\n", testType, err)
	}

	// Wait a moment for terminal to process the signal and close gracefully
	time.Sleep(2 * time.Second)

	return string(output), err
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

// generateMainGoHash generates hash for main.go and main_test.go
func generateMainGoHash(taskDir string) (string, error) {
	mainGoFile := filepath.Join(taskDir, "main.go")
	testFile := filepath.Join(taskDir, "main_test.go")

	mainHash, err := calculateFileHash(mainGoFile)
	if err != nil {
		return "", fmt.Errorf("error hashing main.go: %w", err)
	}

	testHash, err := calculateFileHash(testFile)
	if err != nil {
		return "", fmt.Errorf("error hashing main_test.go: %w", err)
	}

	combinedString := mainHash + testHash
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

// MainCoverageResult holds the result of main.go coverage analysis
type MainCoverageResult struct {
	LineCoverage   float64
	BranchCoverage float64
	CoverageReport string
	Cached         bool
}

// runMainCoverageAnalysis runs coverage analysis on main.go if it exists
func runMainCoverageAnalysis(taskDir string) MainCoverageResult {
	mainGoFile := filepath.Join(taskDir, "main.go")
	testFile := filepath.Join(taskDir, "main_test.go")
	cacheFile := filepath.Join(taskDir, "main_coverage.cache")
	resultFile := filepath.Join(taskDir, "main_coverage_result.txt")

	result := MainCoverageResult{
		LineCoverage:   0.0,
		BranchCoverage: 0.0,
		CoverageReport: "",
		Cached:         false,
	}

	// Check if both main.go and main_test.go exist
	if _, err := os.Stat(mainGoFile); os.IsNotExist(err) {
		result.CoverageReport = "main.go not found - skipping coverage analysis"
		return result
	}

	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		result.CoverageReport = "main_test.go not found - skipping coverage analysis"
		return result
	}

	// Generate current hash
	currentHash, err := generateMainGoHash(taskDir)
	if err != nil {
		result.CoverageReport = fmt.Sprintf("Failed to generate hash: %v", err)
		return result
	}

	// Check if hash matches cached version
	if cachedHash, err := readHashCache(cacheFile); err == nil && cachedHash == currentHash {
		// Hash matches, try to load cached result
		if data, err := os.ReadFile(resultFile); err == nil {
			result.CoverageReport = string(data)
			result.Cached = true

			// Parse cached coverage values
			result.LineCoverage = parseCoverageReport(result.CoverageReport)
			if branchCov, _ := parseGobcoCoverage(result.CoverageReport); branchCov > 0 {
				result.BranchCoverage = branchCov
			}

			fmt.Printf("🚀 main.go coverage - Using cached result (hash match)\n")
			return result
		}
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

	// Run line coverage analysis with terminal and screenshot
	fmt.Printf("📊 Running line coverage analysis for main.go...\n")
	if lineOutput, err := openTerminalAndRunMainTest(taskDir, "line_coverage"); err == nil {
		coverageOutput.WriteString("=== Line Coverage Analysis ===\n")
		coverageOutput.WriteString(lineOutput)
		lineCoverage = parseCoverageReport(lineOutput)
	} else {
		coverageOutput.WriteString(fmt.Sprintf("Line coverage analysis failed: %v\n", err))
	}

	// Run branch coverage analysis with terminal and screenshot
	fmt.Printf("📊 Running branch coverage analysis for main.go...\n")
	if branchOutput, err := openTerminalAndRunMainTest(taskDir, "branch_coverage"); err == nil {
		coverageOutput.WriteString("\n=== Branch Coverage Analysis (gobco) ===\n")
		coverageOutput.WriteString(branchOutput)

		branchCov, coverageReport := parseGobcoCoverage(branchOutput)
		branchCoverage = branchCov
		if coverageReport != "" {
			coverageOutput.WriteString("\n")
			coverageOutput.WriteString(coverageReport)
		}
	} else {
		coverageOutput.WriteString(fmt.Sprintf("\nBranch coverage analysis failed (gobco): %v\n", err))
		coverageOutput.WriteString("Note: Make sure 'gobco' is installed: go install github.com/rillig/gobco@latest\n")
	}

	result.LineCoverage = lineCoverage
	result.BranchCoverage = branchCoverage
	result.CoverageReport = coverageOutput.String()

	// Cache the result
	if err := os.WriteFile(resultFile, []byte(result.CoverageReport), 0644); err != nil {
		fmt.Printf("Warning: Could not write main coverage result file: %v\n", err)
	}

	if err := writeHashCache(cacheFile, currentHash); err != nil {
		fmt.Printf("Warning: Could not write main coverage hash cache: %v\n", err)
	}

	return result
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
func runGoTest(responseFile, testFile, workDir, taskDir string) TestResult {
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

	// Open terminal and run test
	result, err := openTerminalAndRunTest(responseFile, testFile, workDir, responseName)
	if err != nil {
		result.Output = fmt.Sprintf("Failed to run test in terminal: %v", err)
		return result
	}

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
func writeResults(resultsFile string, taskID string, results []TestResult, mainCoverage MainCoverageResult) error {
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
	if mainCoverage.LineCoverage > 0 || mainCoverage.BranchCoverage > 0 || mainCoverage.CoverageReport != "" {
		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 60))
		fmt.Fprintf(writer, "MAIN.GO COVERAGE ANALYSIS")
		if mainCoverage.Cached {
			fmt.Fprintf(writer, " (CACHED)")
		}
		fmt.Fprintf(writer, "\n")
		fmt.Fprintf(writer, "%s\n", strings.Repeat("=", 60))
		if mainCoverage.LineCoverage > 0 || mainCoverage.BranchCoverage > 0 {
			fmt.Fprintf(writer, "Line Coverage: %.1f%%\n", mainCoverage.LineCoverage)
			fmt.Fprintf(writer, "Branch Coverage: %.1f%%\n", mainCoverage.BranchCoverage)
		}
		fmt.Fprintf(writer, "\n%s\n\n", mainCoverage.CoverageReport)
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
	mainCoverage := runMainCoverageAnalysis(taskDir)

	fmt.Printf("Main line coverage: %.1f%%\n", mainCoverage.LineCoverage)
	fmt.Printf("Main branch coverage: %.1f%%\n", mainCoverage.BranchCoverage)
	if mainCoverage.Cached {
		fmt.Printf("Main coverage analysis: CACHED\n")
	}

	// Test each response
	for _, pair := range responsePairs {
		responseFile := pair[1]
		responseName := filepath.Base(responseFile)
		responseName = strings.TrimSuffix(responseName, ".go")

		fmt.Printf("\n%s\n", strings.Repeat("=", 50))
		fmt.Printf("Testing %s...\n", responseName)
		fmt.Printf("%s\n", strings.Repeat("=", 50))

		result := runGoTest(responseFile, testFile, taskDir, taskDir)
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
	}

	// Write results to file
	resultsFile := filepath.Join(taskDir, "result.txt")
	if err := writeResults(resultsFile, taskID, results, mainCoverage); err != nil {
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
	if mainCoverage.LineCoverage > 0 || mainCoverage.BranchCoverage > 0 {
		fmt.Printf("\nMAIN.GO COVERAGE:\n")
		fmt.Printf("Line Coverage: %.1f%%\n", mainCoverage.LineCoverage)
		fmt.Printf("Branch Coverage: %.1f%%\n", mainCoverage.BranchCoverage)
		if mainCoverage.Cached {
			fmt.Printf("Coverage Analysis: CACHED\n")
		}
	}

	absResultsPath, _ := filepath.Abs(resultsFile)
	fmt.Printf("\nResults written to: %s\n", absResultsPath)

	if passedCount > 0 {
		sendNotification(
			"Testing Complete! 🏆",
			fmt.Sprintf("%d/%d tests passed for Task %s", passedCount, len(results), taskID),
		)
	}

	// Final cleanup: Remove all signal files
	fmt.Printf("\n🧹 Final cleanup of signal files...\n")
	signalPattern := filepath.Join(taskDir, "screenshot_done_*.signal")
	if matches, err := filepath.Glob(signalPattern); err == nil {
		for _, match := range matches {
			if err := os.Remove(match); err == nil {
				fmt.Printf("   Removed: %s\n", filepath.Base(match))
			}
		}
	}
}
