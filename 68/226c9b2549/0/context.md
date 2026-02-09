# Session Context

## User Prompts

### Prompt 1

In this branch we've added tests to setup_test.go.  They're hard to read.  Let's instead replace our expectations with literal multiline strings, nice and obvious.

### Prompt 2

OK, that's part of it.  I'd also like to remove all instances of "does the result contain A? Does the result *not* contain B?"  

Instead, i'd like all those assertions replaced with a single comparison to what the result should be, literally.

