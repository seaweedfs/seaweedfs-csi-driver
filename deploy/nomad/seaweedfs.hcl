# WWW naster: http://seaweedfs-master.service.consul:9333/
# WWW filer: http://seaweedfs-filer.service.consul:8888/

job "seaweedfs" {
  datacenters = ["dc1"]
  type = "service"

  group "seaweedfs-master" {
    count = 3

    constraint {
      attribute = "${attr.unique.hostname}"
      operator  = "regexp"
      # We need static IPs for master servers
      # dc1-n1 - 172.21.100.51
      # dc1-n2 - 172.21.100.52
      # dc1-n3 - 172.21.100.53
      value     = "^dc1-n1|dc1-n2|dc1-n3$"
    }
    
    constraint {
      operator  = "distinct_hosts"
      value     = "true"
    }

    restart {
      attempts = 10
      interval = "5m"
      delay = "25s"
      mode = "delay"
    }
    
    update {
      max_parallel = 1
      stagger      = "5m"
      canary       = 0
    }
    
    migrate {
      min_healthy_time = "2m"
    }
    
    network {
      port "http" {
        static = 9333
      }
      port "grpc" {
        static = 19333
      }
    }

    task "seaweedfs-master" {
      driver = "docker"
      env {
        WEED_MASTER_VOLUME_GROWTH_COPY_1 = "1"
        WEED_MASTER_VOLUME_GROWTH_COPY_2 = "2"
        WEED_MASTER_VOLUME_GROWTH_COPY_OTHER = "1"
      }
      config {
        image = "chrislusf/seaweedfs:latest"
        force_pull = "true"
        network_mode = "host"
        args = [
          "-v=1", "master",
          "-volumeSizeLimitMB=100",
          "-resumeState=false",
          "-ip=${NOMAD_IP_http}",
          "-port=${NOMAD_PORT_http}",
          "-peers=172.21.100.51:${NOMAD_PORT_http},172.21.100.52:${NOMAD_PORT_http},172.21.100.53:${NOMAD_PORT_http}",
          "-mdir=${NOMAD_TASK_DIR}/master"
        ]
      }

      resources {
        cpu = 128
        memory = 128
      }
      
      service {
        tags = ["${node.unique.name}"]
        name = "seaweedfs-master"
        port = "http"
        check {
          type = "tcp"
          port = "http"
          interval = "10s"
          timeout = "2s"
        }
      }
    }
  }
  
  
  
  
  group "seaweedfs-volume" {
    count = 3

    constraint {
      attribute = "${attr.unique.hostname}"
      operator  = "regexp"
      # We want to store data on predictive servers
      value     = "^dc1-n1|dc1-n2|dc1-n3$"
    }
    
    constraint {
      operator  = "distinct_hosts"
      value     = "true"
    }

    restart {
      attempts = 10
      interval = "5m"
      delay = "25s"
      mode = "delay"
    }
    
    update {
      max_parallel      = 1
      stagger           = "2m"
    }
    
    migrate {
      min_healthy_time = "2m"
    }
    
    network {
      port "http" {
        static = 8082
      }
      port "grpc" {
        static = 18082
      }
    }

    task "seaweedfs-volume" {
      driver = "docker"
      user = "1000:1000"
        
      config {
        image = "chrislusf/seaweedfs:latest"
        force_pull = "true"
        network_mode = "host"
        args = [
          "volume",
          "-dataCenter=${NOMAD_DC}",
#          "-rack=${meta.rack}",
          "-rack=${node.unique.name}",
          "-mserver=seaweedfs-master.service.consul:9333",
          "-port=${NOMAD_PORT_http}",
          "-ip=${NOMAD_IP_http}",
          "-publicUrl=${NOMAD_ADDR_http}",
          "-preStopSeconds=1",
          "-dir=/data"
        ]
        
        mounts = [
          {
            type = "bind"
            source = "/data/seaweedfs-volume-data" # there should be directory in host VM
            target = "/data"
            readonly = false
            bind_options = {
              propagation = "rprivate"
            }
          }
         ]
      }

      resources {
        cpu = 512
        memory = 2048
        memory_max = 4096 # W need to have memory oversubscription enabled
      }
      
      service {
        tags = ["${node.unique.name}"]
        name = "seaweedfs-volume"
        port = "http"
        check {
          type = "tcp"
          port = "http"
          interval = "10s"
          timeout = "2s"
        }
      }
    }
  }


  group "seaweedfs-filer" {
    count = 1

    constraint {
      operator  = "distinct_hosts"
      value     = "true"
    }

    restart {
      attempts = 10
      interval = "5m"
      delay = "25s"
      mode = "delay"
    }
    
    migrate {
      min_healthy_time = "2m"
    }
    
    network {
      port "http" {
        static = 8888
      }
      port "grpc" {
        static = 18888
      }
      port "s3" {
        static = 8333
      }
    }

    task "seaweedfs-filer" {
      driver = "docker"
      user = "1000:1000"
        
      config {
        image = "chrislusf/seaweedfs:latest"
        force_pull = "true"
        network_mode = "host"
        args = [
          "filer",
          "-dataCenter=${NOMAD_DC}",
#          "-rack=${meta.rack}",
          "-rack=${node.unique.name}",
          "-defaultReplicaPlacement=000",
          "-master=seaweedfs-master.service.consul:9333",
          "-s3",
          "-ip=${NOMAD_IP_http}",
          "-port=${NOMAD_PORT_http}",
          "-s3.port=${NOMAD_PORT_s3}"
        ]
        mounts = [
          {
            type = "bind"
            source = "local/filer.toml"
            target = "/etc/seaweedfs/filer.toml"
          }
        ]

      }
      
      template {
        destination = "local/filer.toml"
        change_mode = "restart"
        data = <<EOH
[postgres2]
enabled = true
createTable = """
  CREATE TABLE IF NOT EXISTS "%s" (
    dirhash   BIGINT,
    name      VARCHAR(65535),
    directory VARCHAR(65535),
    meta      bytea,
    PRIMARY KEY (dirhash, name)
  );
"""
hostname = "172.21.100.54"
port = 5432
username = "seaweedfs"
password = "pass1234567"
database = "seaweedfs"
schema = ""
sslmode = "disable"
connection_max_idle = 100
connection_max_open = 100
connection_max_lifetime_seconds = 0
enableUpsert = true
upsertQuery = """INSERT INTO "%[1]s" (dirhash,name,directory,meta) VALUES($1,$2,$3,$4) ON CONFLICT (dirhash,name) DO UPDATE SET meta = EXCLUDED.meta WHERE "%[1]s".meta != EXCLUDED.meta"""

# ssh ubuntu@172.21.100.54
# sudo -u postgres psql -c "CREATE ROLE seaweedfs WITH PASSWORD 'pass1234567';"
# sudo -u postgres psql -c "CREATE DATABASE seaweedfs OWNER seaweedfs;"
EOH
      }

      resources {
        cpu = 512
        memory = 256
      }
      
      service {
        tags = ["${node.unique.name}"]
        name = "seaweedfs-filer"
        port = "http"
        check {
          type = "tcp"
          port = "http"
          interval = "10s"
          timeout = "2s"
        }
      }
      
      service {
        tags = ["${node.unique.name}"]
        name = "seaweedfs-s3"
        port = "s3"
        check {
          type = "tcp"
          port = "s3"
          interval = "10s"
          timeout = "2s"
        }
      }
    }
  }
  
}
