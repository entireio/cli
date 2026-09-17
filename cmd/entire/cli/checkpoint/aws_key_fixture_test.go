package checkpoint

// awsKeyFixture is an AWS-Access-Key-ID-shaped value for exercising redaction of
// checkpoint content. It is assembled rather than written as one literal so the
// pattern stays off disk; see redact.awsKeyFixture in redact/redact_test.go for
// why that matters. Duplicated here only because that one is package-private.
// Do not re-inline it.
const awsKeyFixture = "AKIAYRWQG5" + "EJLPZLBYNP"
