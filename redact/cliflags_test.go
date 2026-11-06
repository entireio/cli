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
		{name: "quoted operator inside an earlier argument", input: "mysql --execute='select 1; select 2' -p" + p, want: "mysql --execute='select 1; select 2' -pREDACTED"},
		{name: "docker login glued short flag", input: "docker login -u me -p" + p + " registry.example", want: "docker login -u me -pREDACTED registry.example"},
		{name: "login password shaped like a port mapping", input: "docker login -u me -p 1234:5678 registry.example", want: "docker login -u me -p REDACTED registry.example"},
		{name: "required value starting with a dash", input: "sshpass -p -" + p + " ssh host", want: "sshpass -p REDACTED ssh host"},
		{name: "sshpass quoted prompt", input: "sshpass -P 'Password for user:' -p " + p + " ssh host", want: "sshpass -P 'Password for user:' -p REDACTED ssh host"},
		{name: "openssl quoted pass source", input: "openssl enc -aes-256-cbc -pass 'pass:" + p + "'", want: "openssl enc -aes-256-cbc -pass 'pass:REDACTED'"},
		{name: "openssl second pass source", input: "openssl pkcs12 -in c.p12 -passin pass:" + p + " -passout pass:" + p + "2", want: "openssl pkcs12 -in c.p12 -passin pass:REDACTED -passout pass:REDACTED"},
		{name: "substitution printing a literal", input: `docker login -u me -p "$(printf ` + p + `)" ghcr.io`, want: `docker login -u me -p "REDACTED" ghcr.io`},
		{name: "netrc password before login", input: "machine h password " + p + " login alice", want: "machine h password REDACTED login alice"},
		{name: "netrc entries on one line", input: "machine a login me password " + p + " machine b login me password " + p + "2", want: "machine a login me password REDACTED machine b login me password REDACTED"},
		{name: "quoted piece joined to a bare piece", input: "docker login -u me -p 'alpha'" + p + " ghcr.io", want: "docker login -u me -p REDACTED ghcr.io"},
		{name: "two joined quoted pieces", input: `tool --password='abc'"` + p + `"`, want: "tool --password=REDACTED"},
		{name: "bare piece joined to a closing quoted piece", input: `mysql -pSummer"2026 Release"`, want: "mysql -pREDACTED"},
		{name: "escaped quote inside double quotes", input: `mysql -p"abc\"` + p + `" db`, want: `mysql -p"REDACTED" db`},
		{name: "escaped space", input: `mysql -pabc\ ` + p + ` -e x`, want: "mysql -pREDACTED -e x"},
		{name: "command inside a JSON string", input: `{"cmd":"mysql -uroot -p` + p + `","cwd":"/app"}`, want: `{"cmd":"mysql -uroot -pREDACTED","cwd":"/app"}`},
		{name: "JSON-escaped quotes", input: `{"cmd":"mysql -p\"Summer 2026\" db"}`, want: `{"cmd":"mysql -p\"REDACTED\" db"}`},
		{name: "command quoted for sh -c", input: `sh -c "mysql -p` + p + `"; echo " done"`, want: `sh -c "mysql -pREDACTED"; echo " done"`},
		{name: "continued line before the flag", input: "openssl enc -aes-128-cbc \\\n  -pass pass:" + p, want: "openssl enc -aes-128-cbc \\\n  -pass pass:REDACTED"},
		{name: "continued line between flag and value", input: "redis-cli -a \\\n " + p + " ping", want: "redis-cli -a \\\n REDACTED ping"},
		{name: "continued login command", input: "docker login \\\n  -u me \\\n  -p " + p + " ghcr.io", want: "docker login \\\n  -u me \\\n  -p REDACTED ghcr.io"},
		{name: "redis password shaped like a port mapping", input: "redis-cli -a 1234:5678 ping", want: "redis-cli -a REDACTED ping"},
		{name: "openssl password shaped like a port mapping", input: "openssl enc -aes-128-cbc -pass pass:1234:5678", want: "openssl enc -aes-128-cbc -pass pass:REDACTED"},
		{name: "quoted lowercase password after a blank", input: `mongosh --password "swordfish" db`, want: `mongosh --password "REDACTED" db`},
		{name: "literal suffix after a substitution", input: "tool --password=$(gh auth token)" + p, want: "tool --password=REDACTED"},
		{name: "openssl pass source joined to a bare piece", input: "openssl enc -pass 'pass:abc'" + p, want: "openssl enc -pass 'pass:REDACTED"},
		{name: "quoted value with a leading blank", input: "docker login -u me -p ' " + p + "' ghcr.io", want: "docker login -u me -p 'REDACTED' ghcr.io"},
		{name: "escaped shell operator", input: `mysql -pabc\;` + p + " db", want: "mysql -pREDACTED db"},
		{name: "empty quoted prefix", input: "mysql -p''" + p + " db", want: "mysql -pREDACTED db"},
		{name: "bare, quoted and bare pieces", input: `mysql -pabc"` + p + `"xyz db`, want: "mysql -pREDACTED db"},
		{name: "container named like the client", input: "docker exec sshpass sshpass -p " + p + " ssh host", want: "docker exec sshpass sshpass -p REDACTED ssh host"},
		{name: "quoted suffix after a substitution", input: "mysql -p$(cat /run/secrets/pw)'" + p + "' db", want: "mysql -pREDACTED db"},
		{name: "nested substitution", input: "mysql -p$(printf %s $(printf x) " + p + ") db", want: "mysql -pREDACTED db"},
		{name: "literal prefix before a substitution", input: "mysql -pabc$(printf " + p + ") db", want: "mysql -pREDACTED db"},
		{name: "flag-like text inside a quoted argument", input: "mysql -p" + p + " -e 'SELECT -pDebug'", want: "mysql -pREDACTED -e 'SELECT -pDebug'"},
		{name: "netrc account before login", input: "machine h account team login alice password " + p, want: "machine h account team login alice password REDACTED"},
		{name: "netrc comment between fields", input: "machine 127.0.0.1\n login alice\n # build account\n password " + p + "\n", want: "machine 127.0.0.1\n login alice\n # build account\n password REDACTED\n"},
		{name: "dash-leading value after equals", input: "wget --password=-" + p + " https://example.com", want: "wget --password=REDACTED https://example.com"},
		{name: "openssl quoted source with blanks", input: "openssl enc -aes-128-cbc -pass 'pass: " + p + " '", want: "openssl enc -aes-128-cbc -pass 'pass:REDACTED'"},
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
		"mongosh -u admin -p --host db.example.com",
		"openssl enc -aes-256-cbc -pass env:ENC_PASS -in f",
		"openssl rsa -in key.pem -passin file:/run/secrets/key",
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
		"docker run --name mysql -p3306 mysql:8",
		"docker run --name=mongo -p 27017 mongo",
		"sshpass -e ssh -p 2222 user@host",
		"sshpass -f ~/.pw ssh -p 2222 user@host",
		// prose and docs
		"Pass --password to set it, or --password-stdin to read it.",
		"the machine learning password policy",
		"By default login with password is disabled.",
		"mongosh --password swordfish is how the docs spell it",
		`{"cmd":"mysql -uroot -p","cwd":"/app"}`,
		`{"cmd":"mysql -uroot -p", "cwd":"/app"}`,
		`cmd := "mysql -uroot -p" + password + " app"`,
		`cmd := "mysql -uroot -p"+password+" app"`,
		"mysql -u root -p$(cat /run/secrets/pw) db",
		"mysql -u root -e 'select 1' -p",
		"docker run --name mysql \\\n  -p 3306:3306 mysql:8",
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
