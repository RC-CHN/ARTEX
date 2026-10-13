#!/bin/sh
# npm exec queries registry metadata before checking global packages. Dispatch
# the preinstalled tool names directly so existing npx commands work offline;
# leave every other package/version/option to npm's original implementation.
case "${1-}" in
  @playwright/mcp)
    shift
    exec /opt/artex-tools/node_modules/.bin/playwright-mcp "$@"
    ;;
  @playwright/cli)
    shift
    exec /opt/artex-tools/node_modules/.bin/playwright-cli "$@"
    ;;
  playwright)
    shift
    exec /opt/artex-tools/node_modules/.bin/playwright "$@"
    ;;
  *)
    exec /usr/local/bin/node /usr/local/lib/node_modules/npm/bin/npx-cli.js "$@"
    ;;
esac
