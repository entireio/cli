package redact

import "testing"

// A human-chosen password is below entropyThreshold, so only the command-line
// layer can catch it.
const cliTestPassword = "Summer2026Release"

func TestCommandLinePasswordIsBelowEntropyThreshold(t *testing.T) {
	t.Parallel()
	if e := shannonEntropy(cliTestPassword); e > entropyThreshold {
		t.Fatalf("password %q has entropy %.3f > %.1f; not an entropy-miss regression case", cliTestPassword, e, entropyThreshold)
	}
}

func TestString_CommandLinePasswords(t *testing.T) {
	t.Parallel()

	p := cliTestPassword
	assertStringRedactionCases(t, []stringRedactionCase{
		{name: "long flag with equals", input: "mysql -u svc --password=" + p + " -h db app", want: "mysql -u svc --password=REDACTED -h db app"},
		{name: "long flag with a blank", input: "mongosh --username admin --password " + p + " db", want: "mongosh --username admin --password REDACTED db"},
		{name: "quoted long flag", input: `psql --password="` + p + `" -U svc`, want: `psql --password="REDACTED" -U svc`},
		{name: "passwd spelling", input: "tool --passwd=" + p, want: "tool --passwd=REDACTED"},
		{name: "mysql glued short flag", input: "mysql -u root -p" + p + " mydb", want: "mysql -u root -pREDACTED mydb"},
		{name: "mysqldump glued short flag", input: "mysqldump -uroot -p'" + p + "' app > dump.sql", want: "mysqldump -uroot -p'REDACTED' app > dump.sql"},
		{name: "docker login", input: "docker login -u me -p " + p + " ghcr.io", want: "docker login -u me -p REDACTED ghcr.io"},
		{name: "helm registry login", input: "helm registry login registry.example.com -u me -p " + p, want: "helm registry login registry.example.com -u me -p REDACTED"},
		{name: "az login", input: "az login --service-principal -u app -p " + p + " --tenant t", want: "az login --service-principal -u app -p REDACTED --tenant t"},
		{name: "mongo short flag", input: "mongodump -u admin -p " + p + " --db app", want: "mongodump -u admin -p REDACTED --db app"},
		{name: "sshpass", input: "sshpass -p " + p + " ssh user@host", want: "sshpass -p REDACTED ssh user@host"},
		{name: "sshpass glued", input: "sshpass -p" + p + " scp f host:", want: "sshpass -pREDACTED scp f host:"},
		{name: "redis-cli", input: "redis-cli -h cache -a " + p + " ping", want: "redis-cli -h cache -a REDACTED ping"},
		{name: "openssl pass source", input: "openssl enc -aes-256-cbc -in f -pass pass:" + p, want: "openssl enc -aes-256-cbc -in f -pass pass:REDACTED"},
		{name: "openssl passin", input: "openssl rsa -in key.pem -passin pass:" + p, want: "openssl rsa -in key.pem -passin pass:REDACTED"},
		{name: "netrc one line", input: "machine api.heroku.com login me@example.com password " + p, want: "machine api.heroku.com login me@example.com password REDACTED"},
		{name: "netrc several lines", input: "machine api.heroku.com\n  login me@example.com\n  password " + p + "\n", want: "machine api.heroku.com\n  login me@example.com\n  password REDACTED\n"},
		{name: "second command keeps its own flag", input: "cd app && mysql -u root -p" + p, want: "cd app && mysql -u root -pREDACTED"},
		{name: "single-quoted dollar is literal", input: "mysql -u root -p'$" + p + "' mydb", want: "mysql -u root -p'REDACTED' mydb"},
		{name: "quoted value starting with a dash", input: `tool --password="-` + p + `"`, want: `tool --password="REDACTED"`},
		{name: "inside markdown code", input: "Run `mysql -u root -p" + p + "` first.", want: "Run `mysql -u root -pREDACTED` first."},
		{name: "inside command substitution", input: "out=$(mysql -u root -p" + p + ")", want: "out=$(mysql -u root -pREDACTED)"},
		{name: "sshpass verbose", input: "sshpass -v -p " + p + " ssh -p 2222 user@host", want: "sshpass -v -p REDACTED ssh -p 2222 user@host"},
		{name: "openssl password alias", input: "openssl pkcs12 -export -in c.pem -password pass:" + p, want: "openssl pkcs12 -export -in c.pem -password pass:REDACTED"},
		{name: "mongo client after the container", input: "docker exec -it mongo mongosh -u root -p " + p, want: "docker exec -it mongo mongosh -u root -p REDACTED"},
		{name: "netrc written by echo", input: `echo "machine h login me password ` + p + `" > ~/.netrc`, want: `echo "machine h login me password REDACTED" > ~/.netrc`},
		{name: "netrc default entry", input: "machine h login a password x9\ndefault login me password " + p, want: "machine h login a password REDACTED\ndefault login me password REDACTED"},
		{name: "lowercase dollar word is not a variable placeholder", input: "mysql -u root -p$ecret2026 mydb", want: "mysql -u root -pREDACTED mydb"},
		{name: "netrc password starting with a dash", input: "machine h login me password -" + p, want: "machine h login me password REDACTED"},
		{name: "netrc login reused as password", input: "machine router.lan login admin password admin", want: "machine router.lan login admin password REDACTED"},
		{name: "netrc entries on one line", input: "machine a login me password " + p + " machine b login me password " + p + "2", want: "machine a login me password REDACTED machine b login me password REDACTED"},
	})
}

func TestString_CommandLinePasswordNonSecretsPreserved(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		// references and prompts
		"mysql -u root -p mydb",
		"mysql -u root -p$MYSQL_PWD mydb",
		"docker login -u me -p $REGISTRY_TOKEN ghcr.io",
		"echo $TOKEN | docker login --password-stdin -u me ghcr.io",
		"pg_dump --password-file=/run/secrets/db app",
		"mongosh --password $(cat /run/secrets/mongo)",
		"redis-cli -a ${REDIS_PASSWORD} ping",
		`mysql -u root -p"$MYSQL_PWD" mydb`,
		`docker login -u me -p "$(gh auth token)" ghcr.io`,
		"tool --password=<your-password>",
		"tool --password=changeme",
		// short flags that are not passwords for these commands
		"docker run -p 8080:80 nginx",
		"ssh -p 2222 user@host",
		"mysql -P 3306 -u root -p db",
		"docker login ghcr.io; docker run -p 8080:80 app",
		// port mappings, and options of the command sshpass runs
		"docker run -d --name mongo -p 27017:27017 mongo:7",
		"podman run --name mysql -p3306:3306 mysql:8",
		"docker run --name mongo -p 127.0.0.1:27017:27017/tcp mongo",
		"sshpass -e ssh -p 2222 user@host",
		"sshpass -f ~/.pw ssh -p 2222 user@host",
		// prose and docs
		"Pass --password to set it, or --password-stdin to read it.",
		"the machine learning password policy",
		"By default login with password is disabled.",
		"Use `mysql`, `docker login -p`, `sshpass -p` and `openssl -pass pass:` with care.",
		// declarations
		"type netrcLine struct {\n\tmachine  string\n\tlogin    string\n\tpassword string\n}",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got := String(input); got != input {
				t.Errorf("String(%q) = %q, want unchanged", input, got)
			}
		})
	}
}
