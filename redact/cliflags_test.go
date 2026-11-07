package redact

import (
	"strings"
	"testing"
)

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
		{name: "netrc entry in a string argument", input: `fmt.Println("machine h login me password ` + p + `", x)`, want: `fmt.Println("machine h login me password REDACTED", x)`},
		{name: "netrc password starting with a dollar sign", input: "machine 127.0.0.1 login alice password $SUMMER2026", want: "machine 127.0.0.1 login alice password REDACTED"},
		{name: "quoted netrc password starting with a dollar sign", input: `machine h login alice password "$(` + p + `)"`, want: `machine h login alice password "REDACTED"`},
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
		{name: "JSON-escaped quoted argument with a semicolon", input: `{"cmd":"mysql -e \"select 1; select 2\" -p` + p + `"}`, want: `{"cmd":"mysql -e \"select 1; select 2\" -pREDACTED"}`},
		{name: "quoted password between plus signs", input: "mysql -p'+" + p + "+' app", want: "mysql -p'REDACTED' app"},
		{name: "quoted password between dots", input: "mysql -p'." + p + ".' app", want: "mysql -p'REDACTED' app"},
		{name: "client run through sh -c in a container", input: "docker exec mysql sh -c 'mysql -p" + p + " app'", want: "docker exec mysql sh -c 'mysql -pREDACTED app'"},
		{name: "apostrophe earlier on the line", input: "Let's connect with mysql -u root -p'" + p + "' app", want: "Let's connect with mysql -u root -p'REDACTED' app"},
		{name: "netrc escaped quote", input: `machine 127.0.0.1 login alice password "abc\"` + p + `"`, want: `machine 127.0.0.1 login alice password "REDACTED"`},
		{name: "netrc quoted login with a blank", input: `machine 127.0.0.1 login "alice smith" password ` + p, want: `machine 127.0.0.1 login "alice smith" password REDACTED`},
		{name: "netrc quoted machine", input: `machine "my host" login alice password ` + p, want: `machine "my host" login alice password REDACTED`},
		{name: "quoted openssl path", input: `"/usr/bin/openssl" enc -aes-128-cbc -pass "pass:` + p + `"`, want: `"/usr/bin/openssl" enc -aes-128-cbc -pass "pass:REDACTED"`},
		{name: "quoted mysql path", input: `"/usr/bin/mysql" -p"` + p + `"`, want: `"/usr/bin/mysql" -p"REDACTED"`},
		{name: "quoted Windows path", input: `"C:\Program Files\MySQL\bin\mysql.exe" -u root -p"` + p + `"`, want: `"C:\Program Files\MySQL\bin\mysql.exe" -u root -p"REDACTED"`},
		{name: "quoted SQL inside sh -c", input: `sh -c "mysql -p` + p + ` -e 'select -p1'"`, want: `sh -c "mysql -pREDACTED -e 'select -p1'"`},
		{name: "apostrophe before a quoted password with a semicolon", input: "Let's connect with mysql -p'abc;" + p + "' app", want: "Let's connect with mysql -p'REDACTED' app"},
		{name: "escaped quotes inside sh -c", input: `sh -c "mysql -e \"a; b\" -p` + p + `"`, want: `sh -c "mysql -e \"a; b\" -pREDACTED"`},
		{name: "nested command after cd and sudo", input: "docker exec mysql sh -c 'cd /app && sudo mysql -p" + p + " app'", want: "docker exec mysql sh -c 'cd /app && sudo mysql -pREDACTED app'"},
		{name: "nested command after sudo options", input: "docker exec mysql sh -c 'sudo -u root mysql -p" + p + "'", want: "docker exec mysql sh -c 'sudo -u root mysql -pREDACTED'"},
		{name: "nested command after a sudo user named like the client", input: "docker exec mysql sh -c 'sudo -u mysql -E mysql -p" + p + "'", want: "docker exec mysql sh -c 'sudo -u mysql -E mysql -pREDACTED'"},
		{name: "nested command in an if", input: "docker exec mysql sh -c 'if true; then mysql -p" + p + "; fi'", want: "docker exec mysql sh -c 'if true; then mysql -pREDACTED; fi'"},
		{name: "nested command in a loop", input: "docker exec mysql sh -c 'while true; do mysql -p" + p + "; done'", want: "docker exec mysql sh -c 'while true; do mysql -pREDACTED; done'"},
		{name: "sshpass after many sshpass names", input: strings.Repeat("sshpass ", 2000) + "-p " + p + " ssh host", want: strings.Repeat("sshpass ", 2000) + "-p REDACTED ssh host"},
		{name: "nested command after env options", input: "docker exec mysql sh -c 'env -i PATH=/bin mysql -p" + p + "'", want: "docker exec mysql sh -c 'env -i PATH=/bin mysql -pREDACTED'"},
		{name: "escaped quote and semicolon in an earlier argument", input: `openssl enc -out "abc\";x" -pass pass:` + p, want: `openssl enc -out "abc\";x" -pass pass:REDACTED`},
		{name: "line continuation inside the password", input: "openssl enc -pass pass:Summer\\\n" + p + " -in f", want: "openssl enc -pass pass:REDACTED -in f"},
		{name: "line continuation after the password", input: "mysql -p" + p + "\\\n  -h db app", want: "mysql -pREDACTED\\\n  -h db app"},
		{name: "quoted program path with a blank in a directory", input: `"/opt/mysql tools/bin/mysql" -u root -p` + p + ` app`, want: `"/opt/mysql tools/bin/mysql" -u root -pREDACTED app`},
		{name: "relative quoted program path with a blank in a directory", input: `"mysql tools/bin/mysql" -p` + p, want: `"mysql tools/bin/mysql" -pREDACTED`},
		{name: "line continuation inside a quoted password", input: "openssl enc -pass \"pass:Summer\\\n2026Release\" -in f", want: "openssl enc -pass \"pass:REDACTED\" -in f"},
		{name: "semicolon in a command substitution in an earlier argument", input: "openssl enc -in $(printf /dev/null; true) -pass pass:" + p, want: "openssl enc -in $(printf /dev/null; true) -pass pass:REDACTED"},
		{name: "client run in a command substitution of the same client", input: "mysql -h $(cat host; mysql -p" + p + " -e 'select 1') app", want: "mysql -h $(cat host; mysql -pREDACTED -e 'select 1') app"},
		{name: "substitution opener inside a quoted SQL string", input: `mysql -e 'SELECT "$(' -p` + p + ` -e 'SELECT ")"'`, want: `mysql -e 'SELECT "$(' -pREDACTED -e 'SELECT ")"'`},
		{name: "parenthesis in a quoted string of a substitution", input: "mysql -e $(printf ')') -p" + p + " app", want: "mysql -e $(printf ')') -pREDACTED app"},
		{name: "client in a substitution inside a nested command", input: `mysql -h "$(mysql -h $(mysql -p` + p + `))"`, want: `mysql -h "$(mysql -h $(mysql -pREDACTED))"`},
		{name: "client in a quoted argument of a substitution", input: "mysql -e $(sh -c 'mysql -p" + p + "') app", want: "mysql -e $(sh -c 'mysql -pREDACTED') app"},
		{name: "client in a quoted argument of a quoted substitution", input: `mysql -e "$(sh -c 'mysql -p` + p + `')" app`, want: `mysql -e "$(sh -c 'mysql -pREDACTED')" app`},
		{name: "client run through ssh in a substitution", input: "mysql -e $(ssh db mysql -p" + p + ") app", want: "mysql -e $(ssh db mysql -pREDACTED) app"},
		{name: "client name as an argument in a substitution", input: "mysql -e $(mysql -h mysql -p" + p + ") app", want: "mysql -e $(mysql -h mysql -pREDACTED) app"},
		{name: "client in a substitution in a quoted argument of a substitution", input: `mysql -e $(sh -c 'mysql -h "$(mysql -p` + p + `)"') app`, want: `mysql -e $(sh -c 'mysql -h "$(mysql -pREDACTED)"') app`},
		{name: "quoted program path in a quoted argument", input: `docker exec mysql sh -c '"/usr/bin/mysql" -p` + p + `'`, want: `docker exec mysql sh -c '"/usr/bin/mysql" -pREDACTED'`},
		{name: "quoted program path in a substitution", input: `mysql -e $("/usr/bin/mysql" -p` + p + `) app`, want: `mysql -e $("/usr/bin/mysql" -pREDACTED) app`},
		{name: "quoted program path with a dotted directory", input: `"/opt/mysql .tools/bin/mysql" -p` + p, want: `"/opt/mysql .tools/bin/mysql" -pREDACTED`},
		{name: "shell script in a quoted argument", input: `docker exec mysql sh -c 'mysql -e "select 1" && bash -c "mysql -p` + p + `"'`, want: `docker exec mysql sh -c 'mysql -e "select 1" && bash -c "mysql -pREDACTED"'`},
		{name: "Windows program name", input: "mysql.exe -u root -p" + p, want: "mysql.exe -u root -pREDACTED"},
		{name: "many client names in one word", input: "mysql -e 'x," + strings.Repeat("mysql,", 4000) + "' -p" + p, want: "mysql -e 'x," + strings.Repeat("mysql,", 4000) + "' -pREDACTED"},
		{name: "many substitutions in one quoted argument", input: "mysql -e '" + strings.Repeat("$(mysql)", 4000) + "' -p" + p, want: "mysql -e '" + strings.Repeat("$(mysql)", 4000) + "' -pREDACTED"},
		{name: "client run through ssh in a quoted argument", input: `docker exec mysql sh -c 'ssh db "mysql -p` + p + `"'`, want: `docker exec mysql sh -c 'ssh db "mysql -pREDACTED"'`},
		{name: "client run through ssh with options in a quoted argument", input: "docker exec mysql sh -c 'ssh -p 2222 -o BatchMode=yes db mysql -p" + p + "'", want: "docker exec mysql sh -c 'ssh -p 2222 -o BatchMode=yes db mysql -pREDACTED'"},
		{name: "shell script with combined options in a quoted argument", input: `docker exec mysql sh -c 'bash -ce "mysql -p` + p + `"'`, want: `docker exec mysql sh -c 'bash -ce "mysql -pREDACTED"'`},
		{name: "ssh host named like a print command", input: "mysql -e $(ssh echo mysql -p" + p + ") app", want: "mysql -e $(ssh echo mysql -pREDACTED) app"},
		{name: "argument named like a print command in a substitution", input: "mysql -e $(docker exec echo mysql -p" + p + ") app", want: "mysql -e $(docker exec echo mysql -pREDACTED) app"},
		{name: "client run through ssh on a host named like it", input: "ssh mysql mysql -p" + p, want: "ssh mysql mysql -pREDACTED"},
		{name: "ssh host named ssh in a substitution", input: "mysql -e $(ssh ssh mysql -p" + p + ") app", want: "mysql -e $(ssh ssh mysql -pREDACTED) app"},
		{name: "ssh host named ssh in a quoted argument", input: "docker exec mysql sh -c 'ssh ssh mysql -p" + p + "'", want: "docker exec mysql sh -c 'ssh ssh mysql -pREDACTED'"},
		{name: "ssh host named ssh", input: "ssh ssh mysql -p" + p, want: "ssh ssh mysql -pREDACTED"},
		{name: "client in a container named ssh", input: "docker exec ssh mysql -p" + p, want: "docker exec ssh mysql -pREDACTED"},
		{name: "client in a container named ssh in a substitution", input: "mysql -e $(docker exec ssh mysql -p" + p + ") app", want: "mysql -e $(docker exec ssh mysql -pREDACTED) app"},
		{name: "long program path in a quoted argument", input: "docker exec mysql sh -c '/" + strings.Repeat("d", 80) + "/bin/mysql -p" + p + "'", want: "docker exec mysql sh -c '/" + strings.Repeat("d", 80) + "/bin/mysql -pREDACTED'"},
		{name: "many client names in one substitution", input: "mysql -e $(echo " + strings.Repeat("mysql ", 4000) + ") -p" + p, want: "mysql -e $(echo " + strings.Repeat("mysql ", 4000) + ") -pREDACTED"},
		{name: "openssl source with a single-quoted password", input: "openssl enc -pass pass:'$SUMMER2026'", want: "openssl enc -pass pass:'REDACTED'"},
		{name: "openssl source with a substitution printing a literal", input: `openssl enc -pass pass:"$(printf ` + p + `)"`, want: `openssl enc -pass pass:"REDACTED"`},
		{name: "docker login after global options", input: `docker --config "/my dir" -D login -u me -p ` + p + " ghcr.io", want: `docker --config "/my dir" -D login -u me -p REDACTED ghcr.io`},
		{name: "quoted docker program path", input: `"/usr/bin/docker" login -u me -p ` + p, want: `"/usr/bin/docker" login -u me -p REDACTED`},
		{name: "docker login after a quoted global option", input: `docker --config="$HOME/.docker" login -u me -p ` + p, want: `docker --config="$HOME/.docker" login -u me -p REDACTED`},
		{name: "docker login after a substituted global option", input: "docker --config $(mktemp -d) login -u me -p " + p, want: "docker --config $(mktemp -d) login -u me -p REDACTED"},
		{name: "helm registry login after an option", input: "helm registry --debug login registry.example -u me -p " + p, want: "helm registry --debug login registry.example -u me -p REDACTED"},
		{name: "docker program name in capitals", input: "docker.EXE login -u me -p " + p, want: "docker.EXE login -u me -p REDACTED"},
		{name: "client run through ssh with a quoted option argument", input: `ssh -o "ProxyJump jump" db mysql -p` + p, want: `ssh -o "ProxyJump jump" db mysql -pREDACTED`},
		{name: "client after an ssh option argument with its name", input: "ssh -l mysql db mysql -p" + p, want: "ssh -l mysql db mysql -pREDACTED"},
		{name: "client after a substitution that ran it", input: `ssh $(mysql -Nse 'select host') mysql -p` + p, want: `ssh $(mysql -Nse 'select host') mysql -pREDACTED`},
		{name: "client in a subshell", input: "(cd app; mysql -p" + p + ")", want: "(cd app; mysql -pREDACTED)"},
		{name: "client after a subshell that ran it", input: "(mysql -e x) mysql -p" + p, want: "(mysql -e x) mysql -pREDACTED"},
		{name: "openssl single-quoted flag", input: "openssl enc -aes-128-cbc '-pass' pass:" + p, want: "openssl enc -aes-128-cbc '-pass' pass:REDACTED"},
		{name: "openssl double-quoted flag", input: `openssl enc "-passin" pass:` + p, want: `openssl enc "-passin" pass:REDACTED`},
		{name: "openssl ANSI-C quoted source", input: "openssl enc -pass $'pass:" + p + "'", want: "openssl enc -pass $'pass:REDACTED'"},
		{name: "mysql ANSI-C quoted password", input: "mysql -e x -p$'" + p + "'", want: "mysql -e x -p$'REDACTED'"},
		{name: "openssl run through env by path", input: "/usr/bin/env openssl enc -pass pass:" + p, want: "/usr/bin/env openssl enc -pass pass:REDACTED"},
		{name: "openssl run through env by path in a script", input: "sh -c '/usr/bin/env openssl enc -pass pass:" + p + "'", want: "sh -c '/usr/bin/env openssl enc -pass pass:REDACTED'"},
		{name: "openssl run through env by path in a nested script", input: `openssl enc -in f -out "$(bash -c '/usr/bin/env openssl enc -pass pass:` + p + `')"`, want: `openssl enc -in f -out "$(bash -c '/usr/bin/env openssl enc -pass pass:REDACTED')"`},
		{name: "long flag after a quoted flag with no value", input: `echo "--password "; wget --password="` + p + `"`, want: `echo "--password "; wget --password="REDACTED"`},
		{name: "long flag inside a password word", input: `wget --password=x\ --password=` + p, want: "wget --password=REDACTED"},
		{name: "short flag inside a password word", input: `mysql -px\ -p` + p, want: "mysql -pREDACTED"},
		{name: "netrc comment after a value", input: "machine h login me # build account\npassword " + p, want: "machine h login me # build account\npassword REDACTED"},
		{name: "netrc password starting with a hash", input: "machine h login me password #" + p, want: "machine h login me password REDACTED"},
	})
}

// The layer reads every flag on its own, so a flag whose value is no
// password does not hide the next one, which other layers may not catch.
func TestCommandLinePasswordsReadFlagAfterRejectedValue(t *testing.T) {
	t.Parallel()

	input := `echo "--password "; wget --password="` + cliTestPassword + `"`
	start := strings.Index(input, cliTestPassword)
	for _, r := range detectCommandLinePasswords(input) {
		if r.start == start && r.end == start+len(cliTestPassword) {
			return
		}
	}
	t.Errorf("detectCommandLinePasswords(%q) found no region for the password", input)
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
		`bash -c "mysql -uroot -p"; echo "done"`,
		"mysql -e 'SELECT mysql -pDebug FROM metrics'",
		"docker exec mysql sh -c 'sudo apt install mysql -pDebug'",
		"docker exec mysql sh -c 'sudo -n echo mysql -pDebug'",
		"sh -c 'mysql --version'; ssh -p2222 'user@host'",
		`"/opt/mysql tools/bin/psql" -p5432`,
		"sh -c 'mysql --execute=/bin/mysql' -p2222",
		"sh -c 'mysql /bin/mysql' -p2222",
		"sh -c 'mysql ./mysql' -p2222",
		`mysql -e 'select "mysql -pDebug"'`,
		"mysql -e $(echo mysql -pDebug) app",
		"mysql -e $(ssh db echo mysql -pDebug) app",
		"docker exec mysql sh -c 'ssh db echo mysql -pDebug'",
		"ssh mysql -p2222",
		"cd /app && ssh mysql -p2222",
		"ssh -i ~/.ssh/key mysql -p2222",
		"sudo ssh -o BatchMode=yes -l admin mysql -p2222",
		"docker exec mysql sh -c 'ssh mysql -p2222'",
		"docker exec mysql sh -c 'ssh -i ~/.ssh/key mysql -p2222'",
		`ssh -i "/tmp/key" mysql -p2222`,
		`docker exec mysql sh -c 'ssh -o "ProxyJump jump host" mysql -p2222'`,
		"mysql -e $(ssh mysql -p2222) app",
		`docker exec mysql sh -c 'mysql -e "select mysql -pDebug"'`,
		`mysql -h "$(printf %s mysql -p3306)"`,
		"ssh mysql.example.com -p2222",
		"ssh -i ~/.ssh/mysql.pem -p2222 user@host",
		`Run "mysql --socket=/run/mysql" through ssh -p2222 host`,
		"mysql -e $(true; ssh -p2222 host) app",
		"mysql -e $(printf x; echo -pDebug) app",
		`mysql -e \"; printf hello\" -phunter2`,
		`netrc := "machine h login me password " + password + "\n"`,
		`cmd := "mongosh --password " + password + " db"`,
		"mysql -u root -p$(cat /run/secrets/pw) db",
		"mysql -u root -e 'select 1' -p",
		"docker run --name mysql \\\n  -p 3306:3306 mysql:8",
		"docker run --name login -p 8080:80 nginx",
		`openssl enc -pass pass:"$SUMMER2026"`,
		"openssl enc -pass pass:",
		`{"cmd":"openssl enc -pass pass:","x":1}`,
		`cmd := "openssl enc -pass pass:" + pw + " -in f"`,
		"ssh -i ~/.ssh/mysql -p2222 user@host",
		"ssh -l mysql -p2222 db",
		"ssh $(mysql -Nse 'SELECT hostname FROM servers LIMIT 1') -p2222",
		"(mysql -e 'select 1') -p2222",
		"machine h login me # password vault",
		"machine h login me # password vault\nmachine x",
		"Use `mysql`, `docker login -p`, `sshpass -p` and `openssl -pass pass:` with care.",
		`fmt.Sprintf("machine %s login %s password %s", host, user, pass)`,
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
