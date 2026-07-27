#!/bin/bash
fabrics=$(maas admin fabrics read | jq -r 'map(select(.name | index("fabric-core") | not) | .id) | join(",")')
for f in ${fabrics//,/ }; do
  maas admin fabric delete $f
done
