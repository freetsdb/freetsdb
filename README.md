# FreeTSDB 

## An Open-Source Time Series Database, and the BEST Open-Source replacement for InfluxDB Enterprise.

FreeTSDB is an open source **time series database** with
**no external dependencies**. It's the BEST Open-Source replacement for InfluxDB Enterprise.
It's useful for recording metrics, events, and performing analytics.

## Features

* Built-in HTTP API, so you don't have to write any server side code to get up and running.
* Data can be tagged, allowing very flexible querying.
* SQL-like query language.
* Simple to install and manage, and fast to get data in and out.
* It aims to answer queries in real-time. That means every data point is
  indexed as it comes in and is immediately available in queries that
  should return in < 100ms.

## Installation

We recommend installing FreeTSDB using one of the pre-built packages. Then start FreeTSDB using:

* `service freetsdb start` if you have installed FreeTSDB using an official Debian or RPM package.
* `systemctl start freetsdb` if you have installed FreeTSDB using an official Debian or RPM package, and are running a distro with `systemd`. For example, Ubuntu 15 or later.
* `$GOPATH/bin/freetsd` if you have built FreeTSDB from source.

## Getting Started

### Create your first database

```
curl -XPOST "http://localhost:8086/query" --data-urlencode "q=CREATE DATABASE mydb"
```

### Insert some data
```
curl -XPOST "http://localhost:8086/write?db=mydb" \
-d 'cpu,host=server01,region=uswest load=42 1434055562000000000'

curl -XPOST "http://localhost:8086/write?db=mydb" \
-d 'cpu,host=server02,region=uswest load=78 1434055562000000000'

curl -XPOST "http://localhost:8086/write?db=mydb" \
-d 'cpu,host=server03,region=useast load=15.4 1434055562000000000'
```

### Query for the data
```JSON
curl -G "http://localhost:8086/query?pretty=true" --data-urlencode "db=mydb" \
--data-urlencode "q=SELECT * FROM cpu WHERE host='server01' AND time < now() - 1d"
```

### Analyze the data
```JSON
curl -G "http://localhost:8086/query?pretty=true" --data-urlencode "db=mydb" \
--data-urlencode "q=SELECT mean(load) FROM cpu WHERE region='uswest'"
```


## Contact US

* Email: [freetsdb@gmail.com](mailto:freetsdb@gmail.com).
* QQ Group: 663274123
