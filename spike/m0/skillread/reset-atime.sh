#!/bin/sh
# Set the access time of each fixture skill to 2020, so a read in the next case shows as a new atime.
touch -a -t 202001010000 /private/tmp/e16s1/store/skills/*/r1/SKILL.md
stat -f '%Sa %N' -t '%Y-%m-%dT%H:%M:%S' /private/tmp/e16s1/store/skills/*/r1/SKILL.md
