//go:build e2e

package e2e

// PromptTemplate defines a deterministic prompt with expected outcomes.
type PromptTemplate struct {
	Name          string   // Unique name for the prompt
	Prompt        string   // The actual prompt text
	ExpectedFiles []string // Files expected to be created/modified
}

// Deterministic prompts designed for predictable outcomes with minimal token usage.

// PromptCreateHelloGo creates a simple Go hello world program.
var PromptCreateHelloGo = PromptTemplate{
	Name: "CreateHelloGo",
	Prompt: `Create a file called hello.go with a simple Go program that prints "Hello, World!".
Requirements:
- Use package main
- Use a main function
- Use fmt.Println to print exactly "Hello, World!"
- Do not add comments, tests, or extra functionality
- Do not create any other files`,
	ExpectedFiles: []string{"hello.go"},
}

// PromptModifyHelloGo modifies the hello.go file to print a different message.
var PromptModifyHelloGo = PromptTemplate{
	Name: "ModifyHelloGo",
	Prompt: `Modify hello.go to print "Hello, E2E Test!" instead of "Hello, World!".
Do not add any other functionality or files.`,
	ExpectedFiles: []string{"hello.go"},
}

// PromptCreateCalculator creates a simple calculator with add/subtract functions.
var PromptCreateCalculator = PromptTemplate{
	Name: "CreateCalculator",
	Prompt: `Create a file called calc.go with two exported functions:
- Add(a, b int) int - returns a + b
- Subtract(a, b int) int - returns a - b
Requirements:
- Use package main
- No comments or documentation
- No main function
- No tests
- No other files`,
	ExpectedFiles: []string{"calc.go"},
}

// PromptCreateConfig creates a simple JSON config file.
var PromptCreateConfig = PromptTemplate{
	Name: "CreateConfig",
	Prompt: `Create a file called config.json with this exact content:
{
  "name": "e2e-test",
  "version": "1.0.0",
  "enabled": true
}
Do not create any other files.`,
	ExpectedFiles: []string{"config.json"},
}

// PromptAddMultiplyFunction adds a multiply function to calc.go.
var PromptAddMultiplyFunction = PromptTemplate{
	Name: "AddMultiplyFunction",
	Prompt: `Add a Multiply function to calc.go that multiplies two integers.
- Signature: Multiply(a, b int) int
- No comments
- No other changes`,
	ExpectedFiles: []string{"calc.go"},
}

// PromptCreateREADME creates a simple README file.
var PromptCreateREADME = PromptTemplate{
	Name: "CreateREADME",
	Prompt: `Create a file called DOCS.md with exactly this content:
# Documentation

This is the documentation file.

## Usage

Run the program with: go run .
`,
	ExpectedFiles: []string{"DOCS.md"},
}

// PromptCommitChanges instructs the agent to commit changes.
// This is used to test agent commits during a turn.
var PromptCommitChanges = PromptTemplate{
	Name: "CommitChanges",
	Prompt: `Stage and commit all changes with the message "Add feature via agent".
Use git add and git commit commands.`,
	ExpectedFiles: []string{},
}
