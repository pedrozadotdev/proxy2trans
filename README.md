# proxy2trans

`proxy2trans` is a lightweight TCP/UDP gateway written in Go that translates
**PROXY Protocol v2** connections into **transparent Layer-3 connections**.

It is designed for environments where a proxy or tunnel provider delivers the
original client address using PROXY Protocol v2, but the downstream firewall or
application needs to receive packets with the **real client source IP and source
port**.

A primary use case is running `proxy2trans` as a RouterOS container between a
service such as Playit and a downstream firewall.

## Architecture

Example:

```text
Internet client
      |
      v
   Playit
      |
      | PROXY Protocol v2
      v
+----------------+
|   proxy2trans  |
|                |
| TCP / UDP      |
| PPv2 parser    |
| IP_TRANSPARENT |
+----------------+
      |
      | Original client
      | source IP + port
      v
   RouterOS
      |
      v
    Firewall
      |
      v
  Application
```

For example:

```text
203.0.113.50:54321
        |
        | Internet
        v
      Playit
        |
        | PROXY Protocol v2
        v
   proxy2trans
        |
        | source preserved:
        | 203.0.113.50:54321
        v
     Firewall
        |
        v
   Web server
```

The downstream firewall therefore sees the public client address instead of the
address of the Playit agent or `proxy2trans`.

## Features

- TCP support
- UDP support
- PROXY Protocol v2 parsing
- Transparent IPv4 source address preservation
- Original source port preservation
- `IP_TRANSPARENT` socket support
- Multiple listeners
- YAML configuration
- Configurable UDP idle timeout
- Configurable logging levels
- Graceful shutdown on `SIGINT` and `SIGTERM`
- Container-friendly deployment
- RouterOS container support
- No application-specific protocol handling

`proxy2trans` is intentionally application-agnostic. It can sit in front of
HTTP/HTTPS services as well as arbitrary TCP or UDP applications.

## Requirements

The host running `proxy2trans` must support:

- Linux transparent sockets
- `IP_TRANSPARENT`
- policy routing
- connection tracking
- routing of replies back through `proxy2trans`

When deployed on RouterOS, RouterOS must be configured so replies belonging to
transparent connections are returned to the `proxy2trans` container.

## Configuration

By default, `proxy2trans` reads:

```text
/etc/proxy2trans/config.yml
```

A different configuration file can be supplied as the first command-line
argument.

Example:

```sh
proxy2trans /path/to/config.yml
```

The configuration defines TCP and UDP listeners, their downstream targets and
other listener-specific options.

Example:

```yaml
log_level: info

listeners:
  - name: http
    protocol: tcp
    listen: ":80"
    target: "10.10.10.10:80"

  - name: https
    protocol: tcp
    listen: ":443"
    target: "10.10.10.10:443"

  - name: vpn
    protocol: udp
    listen: ":10000"
    target: "10.10.10.10:1194"
    idle_timeout: 2m
```

Adjust the listener addresses, ports and targets for your environment.

Supported log levels are:

```text
debug
info
warn
error
```

The default UDP idle timeout is `2m`.

## PROXY Protocol v2

Incoming connections/datagrams are expected to use PROXY Protocol v2.

For TCP, `proxy2trans` parses the PROXY Protocol v2 header and establishes the
downstream transparent connection using the source address and source port
provided by the header.

For UDP, `proxy2trans` uses the PROXY Protocol v2 information to establish an
association between the upstream flow and its transparent downstream flow.
Subsequent UDP datagrams belonging to the association can then be forwarded
without requiring a new PROXY Protocol header for every payload.

The upstream service must therefore be configured to send PROXY Protocol v2.

## Building

Clone the repository and build the container image:

```sh
docker build -t proxy2trans:latest .
```

To run the image on a conventional Docker host, use the appropriate network
capabilities, routing and configuration mount for your environment.

## Installing on MikroTik RouterOS

RouterOS containers use OCI-compatible container images, but a Docker-generated
archive may need conversion before importing it into RouterOS.

### 1. Build the image

From the repository directory:

```sh
docker build -t proxy2trans:latest .
```

### 2. Export the Docker image

```sh
docker save -o proxy2trans.tar proxy2trans:latest
```

### 3. Convert the archive with Skopeo

Convert the Docker-generated OCI archive into a Docker archive suitable for
RouterOS import:

```sh
skopeo copy \
  oci-archive:proxy2trans.tar \
  docker-archive:proxy2trans-routeros.tar:proxy2trans:latest
```

This creates:

```text
proxy2trans-routeros.tar
```

### 4. Upload the image to RouterOS

Upload:

```text
proxy2trans-routeros.tar
```

to RouterOS, for example:

```text
/data/proxy2trans-routeros.tar
```

Also create/upload the proxy2trans configuration so that it can be mounted at:

```text
/etc/proxy2trans
```

The resulting container should have access to:

```text
/etc/proxy2trans/config.yml
```

### 5. Create the container network

Create a RouterOS container network appropriate for your environment.

Example topology:

```text
RouterOS container network
10.255.254.0/29

RouterOS:       10.255.254.1
Playit:         10.255.254.2
proxy2trans:    10.255.254.3
```

The exact addresses are not required by `proxy2trans`; they are only an example.

Create the VETH interface:

```routeros
/interface/veth
add name=proxy2trans address=10.255.254.3/29 gateway=10.255.254.1
```

Attach it to the RouterOS container bridge used for your container network.

For example:

```routeros
/interface/bridge/port
add bridge=containers interface=proxy2trans
```

### 6. Mount the configuration

Assuming the RouterOS configuration directory is:

```text
/data/proxy2trans
```

create a mount:

```routeros
/container/mounts
add name=proxy2trans-config \
    src=/data/proxy2trans \
    dst=/etc/proxy2trans
```

Place:

```text
config.yml
```

inside that RouterOS directory.

### 7. Import the container

Import the archive:

```routeros
/container
add file=/data/proxy2trans-routeros.tar \
    interface=proxy2trans \
    root-dir=/data/containers/proxy2trans \
    mounts=proxy2trans-config \
    start-on-boot=yes \
    name=proxy2trans
```

Wait for RouterOS to finish extracting the image.

Check the container:

```routeros
/container print
```

Then start it if necessary:

```routeros
/container start [find where name="proxy2trans"]
```

View its logs with:

```routeros
/log print where message~"proxy2trans"
```

## RouterOS Transparent Return Routing

Preserving the client source address means the downstream server or firewall
will send its reply toward its normal default gateway.

RouterOS must recognize connections created by `proxy2trans` and route their
return traffic back through the container.

A typical design is:

```text
                    incoming
                       |
Playit -----> proxy2trans
                  |
                  | transparent connection
                  | source = public client
                  v
               RouterOS
                  |
                  v
               Firewall
                  |
                  | reply to public client
                  v
               RouterOS
                  |
                  | policy routing
                  v
             proxy2trans
                  |
                  v
                Playit
```

Create a dedicated routing table:

```routeros
/routing/table
add fib name=to-proxy2trans
```

Create a default route through the proxy container:

```routeros
/ip/route
add comment="proxy2trans transparent return" \
    dst-address=0.0.0.0/0 \
    gateway=10.255.254.3%containers \
    routing-table=to-proxy2trans
```

Mark connections arriving from `proxy2trans`.

For the example `10.255.254.0/29` container network:

```routeros
/ip/firewall/mangle
add action=mark-connection \
    chain=prerouting \
    comment="proxy2trans - mark transparent connections" \
    connection-mark=no-mark \
    connection-state=new \
    in-interface=containers \
    new-connection-mark=proxy2trans_conn \
    src-address=!10.255.254.0/29
```

The source exclusion is important: traffic generated using the container's own
address must not be mistaken for a transparent client connection.

Then mark replies arriving from the downstream firewall:

```routeros
/ip/firewall/mangle
add action=mark-routing \
    chain=prerouting \
    comment="proxy2trans - route replies to proxy" \
    connection-mark=proxy2trans_conn \
    in-interface=gw \
    new-routing-mark=to-proxy2trans \
    passthrough=no
```

In this example:

```text
containers = RouterOS container bridge
gw         = interface toward the downstream firewall
```

Adapt these names and addresses to your RouterOS configuration.

Do not restrict the second rule to TCP connection states if UDP is being used.
The connection mark is what identifies traffic belonging to a transparent
proxy flow.

## RouterOS Firewall

The RouterOS firewall must permit transparent connections from `proxy2trans` to
the downstream firewall.

For example:

```routeros
/ip/firewall/filter
add chain=forward \
    action=accept \
    in-interface=containers \
    out-interface=gw \
    connection-mark=proxy2trans_conn \
    comment="FORWARD - proxy2trans transparent to firewall"
```

It is strongly recommended to prevent the container's own identity from
directly accessing the downstream firewall:

```routeros
/ip/firewall/filter
add chain=forward \
    action=drop \
    in-interface=containers \
    src-address=10.255.254.3 \
    out-interface=gw \
    comment="FORWARD - block proxy2trans identity to firewall"
```

The transparent connection uses the original public client source address and
therefore matches the connection-marked allow rule.

## NAT

Do **not** source-NAT traffic between `proxy2trans` and the downstream firewall.

The purpose of `proxy2trans` is to preserve:

```text
original client IP
original client source port
```

Applying masquerade/SNAT on this path defeats that purpose.

Internet NAT for unrelated RouterOS traffic can remain enabled normally.

## FastTrack

Be careful with RouterOS FastTrack.

Transparent return routing depends on:

- connection tracking
- connection marks
- mangle
- policy routing

FastTracking these connections can bypass processing required by the
transparent return path.

Either avoid FastTrack or explicitly exclude `proxy2trans` connections from it.

## Security

`proxy2trans` intentionally creates packets using source addresses supplied by
PROXY Protocol v2.

Therefore:

- only accept PROXY Protocol traffic from trusted upstream systems;
- isolate the proxy container network;
- do not expose `proxy2trans` listeners directly to untrusted networks;
- restrict what the container itself can access;
- do not apply generic anti-spoofing rules to the transparent path without
  accounting for the intentionally preserved public source addresses;
- keep anti-spoofing protections at the actual network edge.

A forged PROXY Protocol header from an untrusted source could otherwise claim an
arbitrary source address.

## Troubleshooting

### Verify the container

```routeros
/container print detail
```

### Check RouterOS logs

```routeros
/log print where message~"proxy2trans"
```

### Verify connection marks

```routeros
/ip/firewall/connection
print detail where connection-mark=proxy2trans_conn
```

### Check mangle counters

```routeros
/ip/firewall/mangle
print stats
```

Both the connection-mark and routing-mark rules should increment while
transparent connections are active.

### Capture traffic

Capture on the container side:

```routeros
/tool/sniffer/quick interface=containers
```

and on the downstream-firewall interface:

```routeros
/tool/sniffer/quick interface=gw
```

On the downstream side, the source should be the **original public client IP**,
not the Playit or `proxy2trans` container address.

### TCP works but replies never reach the client

Check:

1. the connection has `proxy2trans_conn`;
2. the reply enters RouterOS through the expected downstream interface;
3. the routing-mark rule increments;
4. the `to-proxy2trans` routing table contains the default route;
5. the route resolves through the container interface;
6. FastTrack is not bypassing mangle/policy routing;
7. no SNAT rule is rewriting the transparent connection.

### UDP does not work

Verify that the upstream service is actually sending PROXY Protocol v2 for the
UDP flow and that the RouterOS return-routing rules are not restricted to TCP
states.

## License

This project is licensed under the MIT License.

See [LICENSE](LICENSE).
