#!/usr/bin/env bash

#./solo.test -test.run=none -test.bench=Solo -test.benchtime=10s -enablesign -txindex -dupcheck -port=8902

rm -rf solo.test.tar.gz

go test -ldflags '-w -s' -c -o solo.test

tar -czf solo.test.tar.gz solo.test
#sshpass -p Fuzamei@mac scp solo.test.tar.gz ubuntu@114.132.219.116:/home/ubuntu/solo/
#sshpass -p Fuzamei@mac ssh ubuntu@114.132.219.116 "cd /home/ubuntu/solo && tar -xzf solo.test.tar.gz"
echo "success"
