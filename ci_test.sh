#!/usr/bin/env bash

./diode_race_test config -list
./diode_race_test time

./diode_race_test -diodeaddrs asia.testnet.diode.io:41046 -e2e=false socksd > /dev/null & 

diode_pid=$!
echo "Start diode-cli pid: $diode_pid"

# check whether the diode socks start
# client should connect to valid network in 60 seconds
port=1080
echo "Waiting for TCP localhost $port ..."

limit=60
tries=0
notyet=1
while [[ $notyet -ge 1 ]]; do
    if [[ $tries -ge $limit ]]; then
      echo "failed"
      exit 1
    fi
    
    nc -z localhost $port;
    notyet=$?
    sleep 1

    tries=$((tries + 1))
    echo -ne "."
done

echo "done"

curl -v --socks5-hostname localhost:1080 pi-taipei.diode.link
ret=$?

kill -9 $diode_pid
echo "\nKill diode-cli"
rm ./diode_race_test

exit $ret