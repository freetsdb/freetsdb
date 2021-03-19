*[English](README.md) ∙ [简体中文](README-zh-Hans.md)*

# FreeTSDB 

## An Open-Source Distributed Time Series Database, and the BEST Open-Source replacement for InfluxDB Enterprise.

FreeTSDB is an open source **Distributed Time Series Database** with
**no external dependencies**. It's the BEST Open-Source replacement for **InfluxDB Enterprise**.
It's useful for recording metrics, events, and performing analytics.

## Features

* Built-in HTTP API, so you don't have to write any server side code to get up and running.
* Data can be tagged, allowing very flexible querying.
* SQL-like query language.
* Clustering is supported out of the box, so that you can scale horizontally to handle your data.
* Simple to install and manage, and fast to get data in and out.
* It aims to answer queries in real-time. That means every data point is
  indexed as it comes in and is immediately available in queries that
  should return in < 100ms.

## Architectural overview
An FreeTSDB system consists of two separate software processes: data nodes, and meta nodes. Communication within a cluster looks like this:

![](https://github.com/freetsdb/freetsdb/blob/master/images/FreeTSDB-Arch.jpg)

The meta nodes communicate with each other via a TCP protocol and the Raft consensus protocol that all use port 8089 by default. The meta nodes also expose an HTTP API bound to port 8091 by default that the influxd-ctl command uses.

Data nodes communicate with each other through a TCP protocol that is bound to port 8088. Data nodes communicate with the meta nodes through their HTTP API bound to 8091. 

Within a cluster, all meta nodes must communicate with all other meta nodes. All data nodes must communicate with all other data nodes and all meta nodes.


## Performance
FreeTSDB's performance is same with InfluxDB. For example, on Aliyun(Alibaba Cloud)'s ECS(16 vCPU 32 GiB, Ubuntu 18.04 64bit),  FreeTSDB's writing performance is **386985.67 point/sec**, which is approximately **2.58x** faster ingestion than Aliyun(Alibaba Cloud)'s InfluxDB system(16 vCPU 16G, ~ 150000 point/sec).

![](https://github.com/freetsdb/freetsdb/blob/master/images/Writing-Performance.png)

## Build

### Installing Go

-------------

FreeTSDB requires Go 1.12 or above.

Gvm is a Go version manager. It's useful for installing Go. For instructions
on how to install it see [the gvm page on github](https://github.com/moovweb/gvm).

After installing gvm you can install and set the default go version by
running the following:

    gvm install go1.15.8
    gvm use go1.15.8 --default

### Revision Control Systems

-------------

Go has the ability to import remote packages via revision control systems with the `go get` command.  To ensure that you can retrieve any remote package, be sure to install the following rcs software to your system.
Currently the project only depends on `git`.

* [Install Git](http://git-scm.com/book/en/Getting-Started-Installing-Git)

### Getting the source

------

Setup the project structure and fetch the repo like so:

```bash
    mkdir $HOME/gocodez
    export GOPATH=$HOME/gocodez
    go get github.com/freetsdb/freetsdb
```

You can add the line `export GOPATH=$HOME/gocodez` to your bash/zsh file to be set for every shell instead of having to manually run it everytime.

### Build

-----

Make sure you have Go installed and the project structure as shown above. To then build and install the binaries, run the following command.

```bash
go clean ./...
go install ./...
```

The binaries will be located in `$GOPATH/bin`. Please note that the FreeTSDB binary is named `freetsd`, not `freetsdb`.

## Installation

### Install meta nodes

#### Meta node setup description and requirements

The Installation process sets up **three** meta nodes, with each meta node running on its own server.
**You must have a minimum of three meta nodes in a cluster**. FreeTSDB clusters require at least three meta nodes and an odd number of meta nodes for high availability and redundancy. We don't recommend having more than three meta nodes unless your servers or the communication between the servers have chronic reliability issues.

#### Meta node setup

##### Step 1: Modify the /etc/hosts File

Add your servers’ hostnames and IP addresses to each cluster server’s /etc/hosts file (the hostnames below are representative).

```
<Meta_1_IP> cluster-meta-node-01
<Meta_2_IP> cluster-meta-node-02
<Meta_3_IP> cluster-meta-node-03
```



> Verification steps:
>
> Before proceeding with the installation, verify on each server that the other servers are resolvable. Here is an example set of shell commands using ping:
>
> ping -qc 1 cluster-meta-node-01
>
> ping -qc 1 cluster-meta-node-02
>
> ping -qc 1 cluster-meta-node-03

We highly recommend that each server be able to resolve the IP from the hostname alone as shown here. Resolve any connectivity issues before proceeding with the installation. A healthy cluster requires that every meta node can communicate with every other meta node.

##### Step 2: Set up, configure, and start the meta services

Perform the following steps on each meta server**.

###### I. Download and install the meta service

**The Linux System**

```
wget https://github.com/freetsdb/freetsdb/releases/download/v0.0.2-beta.1/freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
tar -zxvf freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
```

###### II. Edit the configuration file

In ./freetsdb-meta.conf:

Uncomment hostname and set to the full hostname of the meta node.

```
# Hostname advertised by this host for remote addresses.  This must be resolvable by all

other nodes in the cluster

hostname="<cluster-meta-node-0x>"
```





###### III. Start the meta service

On Linux shell console, enter:

```
sudo ./freetsd-meta -config ./freetsdb-meta.conf
```

###### Step 3: Join the meta nodes to the cluster

From one and only one meta node, join all meta nodes including itself. In our example, from **cluster-meta-node-01**, run:

```
freetsd-ctl add-meta cluster-meta-node-01:8091

freetsd-ctl add-meta cluster-meta-node-02:8091

freetsd-ctl add-meta cluster-meta-node-03:8091
```

> Note: Please make sure that you specify the fully qualified host name of the meta node during the join process. Please do not specify localhost as this can cause cluster connection issues.

The expected output is:

```
Added meta node x at cluster-meta-node-0x:8091
```

> Verification steps:
>
> Issue the following command on any meta node:
>
> ```
> freetsd-ctl show
> 
> ```
>
> The expected output is:
>
> ```
> Data Nodes:
>
> Meta Nodes:
> 1      cluster-meta-node-01:8091
> 2      cluster-meta-node-02:8091
> 3      cluster-meta-node-03:8091
>
> ```



Note that your cluster must have at least three meta nodes. If you do not see your meta nodes in the output, please retry adding them to the cluster.

Once your meta nodes are part of your cluster move on to the next steps to set up your data nodes. Please do not continue to the next steps if your meta nodes are not part of the cluster.




### Install data nodes

#### Data node setup description and requirements

The Installation process sets up two data nodes and each data node runs on its own server. You must have a minimum of two data nodes in a cluster. FreeTSDB clusters require at least two data nodes for high availability and redundancy.
Note: that there is no requirement for each data node to run on its own server. However, best practices are to deploy each data node on a dedicated server.


#### Data node setup

##### Step 1: Modify the /etc/hosts file

Add your servers’ hostnames and IP addresses to each cluster server’s /etc/hosts file (the hostnames below are representative).

```
<Data_1_IP> cluster-data-node-01
<Data_2_IP> cluster-data-node-02
```

> Verification steps:
>
> Before proceeding with the installation, verify on each meta and data server that the other servers are resolvable. Here is an example set of shell commands using ping:
>
> ping -qc 1 cluster-meta-node-01
>
> ping -qc 1 cluster-meta-node-02
>
> ping -qc 1 cluster-meta-node-03
>
> ping -qc 1 cluster-data-node-01
>
> ping -qc 1 cluster-data-node-02

We highly recommend that each server be able to resolve the IP from the hostname alone as shown here. Resolve any connectivity issues before proceeding with the installation. A healthy cluster requires that every meta and data node can communicate with every other meta and data node.

##### Step 2: Set up, configure, and start the data node services

Perform the following steps on each data node.

###### I. Download and install the data service

**The Linux System**

```
wget https://github.com/freetsdb/freetsdb/releases/download/v0.0.2-beta.1/freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
tar -zxvf freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
```

###### II. Edit the data node configuration files

First, in ./freetsdb.conf:

Uncomment hostname at the top of the file and set it to the full hostname of the data node.

```
# Change this option to true to disable reporting.

reporting-disabled = false

bind-address = ":8088"

hostname="<cluster-data-node-0x>" 

```



###### III. Start the data service

On Linux shell console, enter:

```
sudo ./freetsd -config ./freetsdb.conf
```

#### Join the data nodes to the cluster

You should join your data nodes to the cluster only when you are adding a brand new node, either during the initial creation of your cluster or when growing the number of data nodes. If you are replacing an existing data node with influxd-ctl update-data, skip the rest of this guide.

On one and only one of the meta nodes that you set up in the previous document, run:

```
freetsd-ctl add-data cluster-data-node-01:8088

freetsd-ctl add-data cluster-data-node-02:8088
```

The expected output is:

```
Added data node y at cluster-data-node-0x:8088

```

Run the add-data command once and only once for each data node you are joining to the cluster.

> Verification steps:
>
> Issue the following command on any meta node:
>
> ```
> freetsd-ctl show
> ```
>
> The expected output is:
>
> ```
> Data Nodes:
> 4      cluster-data-node-01:8088
> 5      cluster-data-node-02:8088
>
> Meta Nodes:
> 1      cluster-meta-node-01:8091
> 2      cluster-meta-node-02:8091
> 3      cluster-meta-node-03:8091
> ```



The output should include every data node that was added to the cluster. The first data node added should have ID=N, where N is equal to one plus the number of meta nodes. In a standard three meta node cluster, the first data node should have ID=4 Subsequently added data nodes should have monotonically increasing IDs. If not, there may be artifacts of a previous cluster in the metastore.

If you do not see your data nodes in the output, please retry adding them to the cluster.

## Getting Started

### Create your first database

```
curl -XPOST "http://cluster-data-node-01:8086/query" --data-urlencode "q=CREATE DATABASE mydb"
```

### Insert some data

```
curl -XPOST "http://cluster-data-node-01:8086/write?db=mydb" \
-d 'cpu,host=server01,region=uswest load=42 1610800206000000000'

curl -XPOST "http://cluster-data-node-01:8086/write?db=mydb" \
-d 'cpu,host=server02,region=uswest load=78 1610800206000000000'

curl -XPOST "http://cluster-data-node-01:8086/write?db=mydb" \
-d 'cpu,host=server03,region=useast load=15.4 1610800206000000000'
```

### Query for the data

```JSON
curl -G "http://cluster-data-node-01:8086/query?pretty=true" --data-urlencode "db=mydb" \
--data-urlencode "q=SELECT * FROM cpu WHERE host='server01' AND time < now() - 1d"
```

### Analyze the data

```JSON
curl -G "http://cluster-data-node-01:8086/query?pretty=true" --data-urlencode "db=mydb" \
--data-urlencode "q=SELECT mean(load) FROM cpu WHERE region='uswest'"
```



## Getting Help

- [Github Issues](https://github.com/freetsdb/freetsdb/issues)

## Contact US

* Email: [freetsdb@gmail.com](mailto:freetsdb@gmail.com).
