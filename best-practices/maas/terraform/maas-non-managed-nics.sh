#!/bin/bash
maas admin interfaces read $1 | jq -c '{nics: map(select(.type == "physical") | select(.tags | index("tf-managed") | not) | .name) | join(",")}'
