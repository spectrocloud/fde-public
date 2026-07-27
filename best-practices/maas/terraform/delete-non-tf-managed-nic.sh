#!/bin/bash
if [ -z $2 ]; then
  echo '{"result": "No unmanaged interfaces to delete"}'
else
  for i in ${2//,/ }; do
    maas admin interface delete $1 $i > /dev/null
  done
  if [ $? -eq 0 ]; then
    echo "{\"result\": \"Interfaces $2 deleted\"}"
  else
    echo "{\"result\": \"Interfaces $2 failed to delete\"}"
  fi
fi
