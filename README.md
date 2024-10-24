![netreap-social](https://github.com/cosmonic-labs/netreap/assets/1687902/681901f1-8717-43d3-87e1-311e7a5baf83)

# Netreap

Netreap is a non-Kubernetes-based tool for handling Cilium across a cluster,
similar to the functionality of [Cilium
Operator](https://docs.cilium.io/en/v1.13/internals/cilium_operator/#cilium-operator-internals).
It was originally designed just to reap orphaned Cilium
[Endpoints](https://docs.cilium.io/en/v1.13/gettingstarted/terminology/#endpoints),
hence the name of `Netreap`. But we loved the name so much we kept it even
though it does more than reaping.

## So why does this exist?

The current [Cilium
Operator](https://github.com/cilium/cilium/tree/master/operator) only works for
Kubernetes and even when we tried to fork it, Kubernetes was too deeply
ingrained to just pull it out, so we created this little project. This helps
clean up nodes that no longer exist from the KV store, and deletes any
endpoints that no longer have services. Ideally, we will want to make this more
generic and open source so other people can take advantage of this work.

## Running

Instructions for running and configuring Netreap are found below. Please note
that Netreap uses leader election, so multiple copies can (and should) be run.

### Installing

#### Requirements

* A Consul cluster or server
* A running Nomad cluster
* Cilium 1.12.x or 1.13.x
  * You will also need to install the [CNI
    plugins](https://github.com/containernetworking/plugins/releases/tag/v1.2.0)
    alongside Cilium

As of v0.2.0 Consul is no longer required for endpoint reconciliation in
Cilium. You may chose to continue to use Consul as Cilum's KV store, but you
can also use etcd. The install guide assumes you want to use Consul as the
kvstore, since you will need it to distribute Cilium policies.

#### Running Cilium

Due to the way Nomad fingerprinting currently works, you _cannot_ run Cilium as
a system job to provide the CNI plugin. This means you'll need to configure and
run it yourself on every agent that you want to include in the Cilium mesh.

##### Iptables

Make sure that iptables is properly configured on the host:

```bash
cat <<'EOF' | sudo tee /etc/modules-load.d/iptables.conf
iptable_nat
iptable_mangle
iptable_raw
iptable_filter
ip6table_mangle
ip6table_raw
ip6table_filter
EOF
```

##### Cilium Agent

Since you can't run Cilium as a Nomad job right now, the easiest way to run it
is to just use systemd. You can run and enable a job similar to the following:

```systemd
[Unit]
Description=Cilium Agent
After=docker.service
Requires=docker.service
After=consul.service
Wants=consul.service
Before=nomad.service

[Service]
Restart=always
ExecStartPre=-/usr/bin/docker exec %n stop
ExecStartPre=-/usr/bin/docker rm %n
ExecStart=/usr/bin/docker run --rm --name %n \
  -v /var/run/cilium:/var/run/cilium \
  -v /sys/fs/bpf:/sys/fs/bpf \
  --net=host \
  --cap-add NET_ADMIN \
  --cap-add NET_RAW \
  --cap-add IPC_LOCK \
  --cap-add SYS_MODULE \
  --cap-add SYS_ADMIN \
  --cap-add SYS_RESOURCE \
  --privileged \
  cilium/cilium:v1.13.1 \
  cilium-agent --kvstore consul --kvstore-opt consul.address=127.0.0.1:8500 \
    --enable-ipv6=false -t geneve \
    --enable-l7-proxy=false  \
    --ipv4-range 172.16.0.0/16

[Install]
WantedBy=multi-user.target
```

Note that this actually runs Cilium with Docker! The reason for this is that
Cilium uses forked versions of some key libraries and needs access to a C
compiler. We found that it is easier to just the container instead of
installing all of Cilium's dependencies.

If you use Consul ACLs, then you will need to add a token to the `Service`
block in the systemd unit so that Cilium can connect to the cluster.

```systemd
[Service]
Environment="CONSUL_HTTP_TOKEN=..."
```

#### Configuring the CNI

The big thing to note is that you need to make sure that the IP CIDR you use
for Cilium does _not_ conflict with what Docker uses if you're using Docker. If
it does or if you want to change Docker's IP range, take a look at the
`default-address-pools` option in `daemon.json`, ex.

```json
{
  "default-address-pools": [
    {
      "base": "192.168.0.0/24",
      "size": 24
    }
  ]
}

```

You will then need to make sure you have a CNI configuration for Cilium in
`/opt/cni/config` named `cilium.conflist`:

```json
{
  "name": "cilium",
  "cniVersion": "1.0.0",
  "plugins": [
     {
       "type": "cilium-cni",
       "enable-debug": false
     }
  ]
}
```

Ensure that the Cilium CNI binary is available in `/opt/cni/bin`:

```bash
sudo docker run --rm --entrypoint bash -v /tmp:/out cilium/cilium:v1.13.1 -c \
  'cp /usr/bin/cilium* /out; cp /opt/cni/bin/cilium-cni /out'
sudo mv /tmp/cilium-cni /opt/cni/bin/cilium-cni
# Optionally install the other Cilium binaries to /usr/local/bin
sudo mv /tmp/cilium* /usr/local/bin

```

### Running Netreap

Run Netreap as a system job in your cluster similar to the following:

```hcl
job "netreap" {
  datacenters = ["dc1"]
  priority    = 100
  type        = "system"

  constraint {
    attribute = "${attr.plugins.cni.version.cilium-cni}"
    operator = "is_set"
  }

  group "netreap" {
    restart {
      interval = "10m"
      attempts = 5
      delay = "15s"
      mode = "delay"
    }
    service {
      name = "netreap"
      tags = ["netreap"]
    }

    task "netreap" {
      driver = "docker"

      config {
        image        = "ghcr.io/cosmonic/netreap:0.2.0"
        network_mode = "host"

        # You must be able to mount volumes from the host system so that
        # Netreap can use the Cilium API over a Unix socket.
        # See
        # https://developer.hashicorp.com/nomad/docs/drivers/docker#plugin-options
        # for more information.
        volumes = [
          "/var/run/cilium:/var/run/cilium"
        ]
      }

    }
  }
}
```

The job constraint ensures that Netreap will only run on nodes where the
Cilium CNI is available.

If you use Nomad or Consul ACLs then you will need to set them in the Netreap
job, ex.

```hcl

      template {
        destination = "secrets/file.env"
        env         = true
        change_mode = "restart"
        data        = <<EOT
CONSUL_HTTP_TOKEN="..."
NOMAD_TOKEN="..."
EOT
      }
```

Note that all environment variables used to configure the Consul and Nomad API
clients are available to Netreap.

### Configuring

| Flag                         | Env Var                        | Default                       | Description                                                                                                   |
| ---------------------------- | ------------------------------ | ----------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `--debug`                    | `NETREAP_DEBUG`                | `false`                       | Turns on debug logging                                                                                        |
| `--policy-key`               | `NETREAP_POLICY_KEY`           | `netreap.io/policy`           | Consul key that Netreap watches for changes to the Cilium policy JSON value                                   |
| `--ignore-nomad-meta-labels` | `NETREAP_IGNORED_META_LABELS`  | `""`                          | Comma-separated list of Nomad's job metadata that not will be passed to Cilium Endpoint labels                |

Please note that to configure the Nomad, Consul and Cilium clients that Netreap uses,
we leverage the well defined environment variables for
[Nomad](https://www.nomadproject.io/docs/commands#environment-variables),
[Consul](https://www.consul.io/commands#environment-variables) and
[Cilium](https://pkg.go.dev/github.com/cilium/cilium/pkg/client#DefaultSockPath).

Right now we only allow connecting to the local Unix socket endpoint for the
Cilium agent. As we determine how we are going to set things up with Cilium, we
can add additional configuration options.

### Cilium Policies

One of Netreap's key responsibilities is to sync [Cilium
policies](https://docs.cilium.io/en/stable/security/policy/) to every node in
your Cilium mesh. Normally Cilium policies are configured using Kubernetes
CRDs, but we don't have that option when we're running Nomad. Normally Cilium
combines all of the CRD values in to a single JSON representation which is
imported by every agent. What this means is that Netreap does the same thing by
watching a single Consul key that stores the complete JSON representation of
all of the Cilium policies in your cluster. The official documentation has
examples on how to write policies in JSON.

Whenever you want to update policies in your cluster, simply set the key in
Consul:

```bash
consul kv put netreap.io/policy @policy.json
```

Netreap automatically picks up any updates to the value and updates the policy
on every node where it is running.

## Development

Netreap is written in pure Go, no other build tools are required other than a
working Go toolchain.

On the other hand, actually using it is a bit more difficult. You need the
following things set up on a Linux machine:

* Consul agent running (no special configuration required, can just use `-dev`
  if you want)
* Nomad configured to use Docker volumes
* Cilium installed using the directions in [Running Cilium](###Running-Cilium).

### Testing

Because of all of the necessary pieces described in the previous section, we
don't have any automated tests in place yet. For now, here are some steps to
test manually:

* Start a job and then start netreap with the `--debug` flag, making sure the
  logs say that it is labeling it
* Run `cilium endpoint list` and make sure the endpoint is showing a label that
  looks something like this: `netreap:job_id=example`
* Stop the job and make sure the logs note that the reap counter was
  incremented
* Start a job and make sure the logs note that it saw the new job. Run `cilium
  endpoint list` to make sure the endpoint was properly labeled
* Stop netreap and then start it again, making sure the logs say that it is
  deleting an endpoint (from the previous job you stopped). Run `cilium
  endpoint list` to make sure the endpoint was properly deleted.
