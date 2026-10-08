@{
    Severity     = @('Error', 'Warning')

    ExcludeRules = @(
        # The installer runs as `irm ... | iex` and its only channel to the
        # user is the host: anything sent to the output stream is returned into
        # the caller's pipeline instead of being displayed, and the coloured
        # status lines need Write-Host. The rule's own escape hatch (functions
        # with the Show verb) does not cover the inline Write-Host calls in the
        # PATH-conflict report, so the rule is excluded for the whole directory.
        'PSAvoidUsingWriteHost'
    )
}
