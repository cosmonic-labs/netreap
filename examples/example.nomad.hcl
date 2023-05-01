job "example" {
  // This will end up as a label on the Cilium endpoint.
  meta = {
    "example_workload/app_name" = "example"
  }

  group "cache" {
    network {
      port "db" {
        to = 6379
      }
      // This selects the CNI plugin to use.
      // The name after "cni/" should match the conflist that is configured on
      // the Nomad node.
      // See https://developer.hashicorp.com/nomad/docs/job-specification/network#cni.
      mode = "cni/cilium"
    }

    service {
      name         = "example"
      port         = "db"
      tags         = ["example"]
      address_mode = "alloc"
    }

    task "redis" {

      driver = "docker"
      config {
        image          = "redis:7"
        ports          = ["db"]
        auth_soft_fail = true
      }

      identity {
        env  = true
        file = true
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
