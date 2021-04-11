## 部署
FreeTSDB原生支持分布式集群能力，FreeTSDB集群的部署主要涉及到META节点和DATA节点，META节点存放的是系统运行所必须的元数据，DATA节点存放的是实际的时序数据。本文档将以3 META节点、2 DATA节点的集群为例演示如何搭建FreeTSDB集群。

### 部署META节点

基于可用性和资源成本的考虑，META节点推荐为3节点。

#### 配置META接地那

##### Step 1: 修改/etc/hosts文件，配置主机名字信息。

将各META节点的名字（比如cluster-meta-node-01）和对应的地址信息（比如<Meta_1_IP>）配置在各META节点和DATA节点的/etc/hosts文件中。

```
<Meta_1_IP> cluster-meta-node-01
<Meta_2_IP> cluster-meta-node-02
<Meta_3_IP> cluster-meta-node-03
```

配置完主机名字信息后，在各META节点和各DATA节点上分别执行如下ping命令，确保配置的正确性和网络的连通性。

>
> ping -qc 1 cluster-meta-node-01
>
> ping -qc 1 cluster-meta-node-02
>
> ping -qc 1 cluster-meta-node-03


##### Step 2: 配置和启动META服务

在各META节点上，分别执行如下命令。

###### I. 下载安装包

比如，在Linux系统上，使用以下命令下载freetsdb-v0.0.2-beta.1的安装包。

```
wget https://github.com/freetsdb/freetsdb/releases/download/v0.0.2-beta.1/freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
tar -zxvf freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
```

###### II. 修改配置

修改配置文件./freetsdb-meta.conf，并将hostname设置为本机的主机名字（比如<cluster-meta-node-0x>）

```
# Hostname advertised by this host for remote addresses.  This must be resolvable by all

other nodes in the cluster

hostname="<cluster-meta-node-0x>"
```



###### III. 启动META服务

在Linux shell命令行中，执行如下命令。

```
sudo ./freetsd-meta -config ./freetsdb-meta.conf
```

###### Step 3: 将META节点加入到集群中

在其中的一个META节点上（比如cluster-meta-node-01），执行如下命令，将已配置好的META节点加入到集群中。

```
freetsd-ctl add-meta cluster-meta-node-01:8091

freetsd-ctl add-meta cluster-meta-node-02:8091

freetsd-ctl add-meta cluster-meta-node-03:8091
```

正确执行时，add-meta命令的执行输出如下：
```
Added meta node x at cluster-meta-node-0x:8091
```

将所有META节点加入到集群后，可以执行“freetsd-ctl show”来查看集群中META节点的信息。
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





### 部署DATA节点

本文档将以2个DATA节点的集群部署为例，演示如何添加DATA节点。

#### 配置DATA节点

##### Step 1: 修改/etc/hosts文件，配置DATA节点的主机名字信息。

将各DATA节点的名字（比如cluster-data-node-01）和对应的地址信息（比如<Data_1_IP>）配置在各META节点和DATA节点的/etc/hosts文件中。

```
<Data_1_IP> cluster-data-node-01
<Data_2_IP> cluster-data-node-02
```

配置完主机名字信息后，在各META节点和DATA节点上分别执行如下ping命令，确保配置的正确性和网络的连通性。
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


##### Step 2: 配置和启动DATA服务

在各DATA节点上，分别执行如下命令。

###### I. 下载安装包

比如，在Linux系统上，使用以下命令下载freetsdb-v0.0.2-beta.1的安装包。

```
wget https://github.com/freetsdb/freetsdb/releases/download/v0.0.2-beta.1/freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
tar -zxvf freetsdb-v0.0.2-beta.1_linux_amd64.tar.gz
```

###### II. Edit the data node configuration files

修改配置文件./freetsdb.conf，并将hostname设置为本机的主机名字（比如<cluster-data-node-0x>）

```
# Change this option to true to disable reporting.

reporting-disabled = false

bind-address = ":8088"

hostname="<cluster-data-node-0x>" 

```



###### III. 启动DATA服务

在Linux shell命令行中，执行如下命令。

```
sudo ./freetsd -config ./freetsdb.conf
```

#### 将DATA节点加入到集群中

在其中的一个META节点上（比如cluster-meta-node-01），执行如下命令，将已配置好的DATA节点加入到集群中。


```
freetsd-ctl add-data cluster-data-node-01:8088

freetsd-ctl add-data cluster-data-node-02:8088
```

正确执行时，add-data命令的执行输出如下：

```
Added data node y at cluster-data-node-0x:8088

```

将所有DATA节点加入到集群后，可以执行“freetsd-ctl show”来查看集群中DATA节点的信息。
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



恭喜你，FreeTSDB集群已搭建完成，欢迎进入FreeTSDB的精彩世界。
