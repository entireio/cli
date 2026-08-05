# VHS script for the onboarding prototype — https://github.com/charmbracelet/vhs
# Record with:  cd prototypes/onboarding && vhs demo.tape   -> onboarding-demo.gif
#
# Demos the repo-no-origin flow: the amber "unleash entire.io" pitch, toggling
# Publish on, then Enter running the mocked publish -> login -> mirror.

Output onboarding-demo.gif

Set FontSize 16
Set Width 1120
Set Height 660
Set Padding 18

# --- setup (hidden): build, make a no-origin git repo, alias `entire` ---
Hide
Type "go build -o /tmp/onb . && d=$(mktemp -d) && cd $d && git init -q && git commit --allow-empty -qm init && entire() { shift; /tmp/onb --fast \"$@\"; } && clear"
Enter
Show

# --- the demo ---
Type "entire enable"
Sleep 500ms
Enter
Sleep 2s

Right          # Publish: No -> Yes  (watch the amber pitch clear + CTA change)
Sleep 2s

Enter          # accept — runs the mocked publish -> login -> mirror
Sleep 4s
