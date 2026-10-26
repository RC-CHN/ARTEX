# Only mounted in the disposable AI fixture: preserve CLI audit instrumentation
# when the native Bash tool starts a login shell and resets PATH.
export PATH="/work/bin:$PATH"
