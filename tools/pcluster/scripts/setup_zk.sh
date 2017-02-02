#!/bin/bash

/zookeeper/bin/zkCli.sh -server 127.0.0.1:2181 <<EOF
create /peloton peloton
create /peloton/master peloton/master
create /peloton/master/leader /peloton/master/leader

create /peloton/resmgr peloton/resmgr
create /peloton/resmgr/leader /peloton/resmgr/leader

EOF
