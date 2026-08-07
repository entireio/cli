package cli

import "fmt"

// The hook scripts below run on every directory change, so the steady state
// must cost nothing. Each one does the whole decision with shell builtins:
//
//  1. bail out unless the shell is interactive;
//  2. bail out if ENTIRE_NO_SHELL_HOOK is set (the global kill switch);
//  3. walk up from $PWD looking for a .git entry (test -e, so a gitfile in a
//     worktree or submodule counts as well as a directory);
//  4. dedupe on the repository root seen last, so moving between directories
//     inside one repository is free;
//  5. bail out if .entire/settings.json or .entire/settings.local.json exists.
//
// Only when all of that passes does the hook fork `entire shellhook check`.
// That command is itself silent unless it has something to say, and always
// exits 0 — see runShellhookCheck.

const shellhookScriptZsh = `[[ -o interactive ]] || return
__entire_hook_check() {
  emulate -L zsh
  [[ -n "$ENTIRE_NO_SHELL_HOOK" ]] && return
  local dir=$PWD
  while [[ -n "$dir" && "$dir" != "/" ]]; do
    if [[ -e "$dir/.git" ]]; then
      [[ "$dir" == "$__ENTIRE_HOOK_LAST_ROOT" ]] && return
      __ENTIRE_HOOK_LAST_ROOT=$dir
      if [[ ! -f "$dir/.entire/settings.json" && ! -f "$dir/.entire/settings.local.json" ]]; then
        command entire shellhook check --root "$dir"
      fi
      return
    fi
    dir=${dir:h}
  done
  __ENTIRE_HOOK_LAST_ROOT=""
}
autoload -Uz add-zsh-hook
add-zsh-hook chpwd __entire_hook_check
__entire_hook_check
`

// Bash has no chpwd hook, so the check hangs off PROMPT_COMMAND with its own
// $PWD dedupe. The wrapper saves and restores $?, or the user's prompt would
// see the hook's exit status instead of their last command's.
const shellhookScriptBash = `case $- in *i*) ;; *) return ;; esac
__entire_hook_check() {
  [ -n "$ENTIRE_NO_SHELL_HOOK" ] && return
  local dir=$PWD
  while [ -n "$dir" ] && [ "$dir" != "/" ]; do
    if [ -e "$dir/.git" ]; then
      [ "$dir" = "$__ENTIRE_HOOK_LAST_ROOT" ] && return
      __ENTIRE_HOOK_LAST_ROOT=$dir
      if [ ! -f "$dir/.entire/settings.json" ] && [ ! -f "$dir/.entire/settings.local.json" ]; then
        command entire shellhook check --root "$dir"
      fi
      return
    fi
    dir=${dir%/*}
  done
  __ENTIRE_HOOK_LAST_ROOT=""
}
__entire_hook_prompt() {
  local __entire_rc=$?
  if [ "$PWD" != "$__ENTIRE_HOOK_LAST_PWD" ]; then
    __ENTIRE_HOOK_LAST_PWD=$PWD
    __entire_hook_check
  fi
  return $__entire_rc
}
case ";$PROMPT_COMMAND;" in
  *";__entire_hook_prompt;"*) ;;
  *) PROMPT_COMMAND="__entire_hook_prompt${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
esac
__entire_hook_prompt
`

// Fish fires --on-variable PWD only on later changes, so the function is also
// called once at load time to cover the shell's starting directory.
const shellhookScriptFish = `status is-interactive; or exit
function __entire_hook_check --on-variable PWD
    if test -n "$ENTIRE_NO_SHELL_HOOK"
        return
    end
    set -l dir $PWD
    while test -n "$dir" -a "$dir" != "/"
        if test -e "$dir/.git"
            if test "$dir" = "$__entire_hook_last_root"
                return
            end
            set -g __entire_hook_last_root $dir
            if not test -f "$dir/.entire/settings.json"; and not test -f "$dir/.entire/settings.local.json"
                command entire shellhook check --root "$dir"
            end
            return
        end
        set dir (string replace -r '/[^/]*$' '' -- $dir)
    end
    set -g __entire_hook_last_root ""
end
__entire_hook_check
`

// shellhookRCLines maps a shell to the single line an rc file needs in order
// to load the hook. Each guards on `entire` being present so a shell on a
// machine where the CLI was uninstalled still starts cleanly.
var shellhookRCLines = map[shellKind]string{
	shellZsh:  `command -v entire >/dev/null 2>&1 && eval "$(entire shellhook init zsh)"`,
	shellBash: `command -v entire >/dev/null 2>&1 && eval "$(entire shellhook init bash)"`,
	shellFish: `type -q entire; and entire shellhook init fish | source`,
}

// shellhookScript returns the hook script for a shell.
func shellhookScript(shell string) (string, error) {
	switch shellKind(shell) {
	case shellZsh:
		return shellhookScriptZsh, nil
	case shellBash:
		return shellhookScriptBash, nil
	case shellFish:
		return shellhookScriptFish, nil
	default:
		return "", fmt.Errorf("%w: %q (supported: %s)", errUnsupportedShell, shell, supportedShellNames)
	}
}
