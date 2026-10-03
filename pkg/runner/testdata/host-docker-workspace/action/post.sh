#!/bin/sh
set -eu
test "$STATE_main" = copied
test "$(cat following)" = following
test "$(cat failed-file)" = failed
test -x added
test ! -e deleted
printf post > post
