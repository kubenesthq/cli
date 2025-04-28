# 1. Register a user. This also creates a default team for the user.
curl -X POST http://localhost:3000/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{
    "email": "team@test.com",
    "first_name": "Team",
    "last_name": "Test",
    "password1": "password123",
    "password2": "password123"
  }'

export JWT_TOKEN=$(curl -X POST http://localhost:3000/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "team@test.com",
    "password": "password123"
  }' | jq -r '.token')

export TEAM_UUID=$(curl -X POST http://localhost:3000/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "team@test.com",
    "password": "password123"
  }' | jq -r '.user.team_uuid')


# 3. Create an SSH provider
curl -X POST http://localhost:3000/api/v1/providers \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "name": "my-ssh-provider",
    "cloud": "ssh"
  }'


# 3. Create a DigitalOcean provider
curl -X POST http://localhost:3000/api/v1/providers \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "name": "my-do-provider",
    "cloud": "digitalocean",
    "api_key": "dop_v1_bbbca60a73d7fbf6849ddab8c32b6ba51e3df7b26a5cbd68fdacadbe694db744"
  }'


# 3. List providers
curl -X GET http://localhost:3000/api/v1/providers \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"

# 4. Get a specific provider
curl -X GET http://localhost:3000/api/v1/providers/PROVIDER_UUID \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"

# 5. Delete a provider
curl -X DELETE http://localhost:3000/api/v1/providers/PROVIDER_UUID \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"


# 3. Create a team (you'll be automatically added as admin)
export TEAM_UUID=$(curl -X POST http://localhost:3000/api/v1/teams \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Engineering Team",
    "description": "Team for engineering staff"
  }' | jq -r '.uuid')

# 4. Register another user to add to the team
curl -X POST http://localhost:3000/api/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{
    "email": "member@example.com",
    "password": "password123"
  }'

# 5. Get the new user's UUID from their login response
export MEMBER_UUID=$(curl -X POST http://localhost:3000/api/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "member@example.com",
    "password": "password123"
  }' | jq -r '.user.uuid')

# 6. Add the new user to the team
curl -X POST http://localhost:3000/api/v1/teams/$TEAM_UUID/members \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"user_uuid\": \"$MEMBER_UUID\",
    \"role\": \"member\"
  }"

# 7. Verify team members
curl -X GET http://localhost:3000/api/v1/teams/$TEAM_UUID \
  -H "Authorization: Bearer $JWT_TOKEN"

## Clusters

### Create Cluster

# Create a new cluster
curl -X POST http://localhost:3000/api/v1/clusters \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "name": "test-cluster",
    "provider_uuid": "$PROVIDER_UUID",
    "type": "standard",
    "region": "blr1",
    "nodes": [
      {
        "name": "control-plane",
        "size": "s-2vcpu-4gb",
        "is_control_plane": true
      }
    ]
  }'

# Get a cluster
curl -X GET http://localhost:3000/api/v1/clusters/CLUSTER_UUID \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"

# Get all clusters
curl -X GET http://localhost:3000/api/v1/clusters \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"


# Add nodes to a cluster

# Remove nodes from a cluster

# Delete a cluster

# Import an existing cluster

curl -X POST 'http://localhost:3000/api/v1/clusters' \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "name": "my-imported-cluster",
    "type": "imported",
    "api_server": "https://my-cluster.example.com:6443"
  }'

# Add kubeconfig

curl -X POST 'http://localhost:3000/api/v1/clusters/kubeconfig' \
  -H "Content-Type: application/yaml" \
  -H "X-Cluster-Key: bw_52cfd4358a1d12349069c4fa986c8e" \
  --data-binary @dummy-kubeconfig.yaml

# Create an invitation (requires team context and auth):

curl -X POST http://localhost:3000/api/v1/invitations \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -H "X-Team-UUID: TEAM_UUID" \
  -d '{
    "team_uuid": "TEAM_UUID",
    "resource_type": "cluster",
    "resource_uuid": "CLUSTER_UUID",
    "email": "user@example.com",
    "role": "ops"
  }'

# Accept an invitation (requires auth):

curl -X POST http://localhost:3000/api/v1/invitations/accept \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -d '{
    "token": "INVITATION_TOKEN"
  }'

# List team's invitations:

curl -X GET http://localhost:3000/api/v1/invitations \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -H "X-Team-UUID: TEAM_UUID"

# List pending invitations for current user:

curl -X GET http://localhost:3000/api/v1/invitations/pending \
  -H "Authorization: Bearer YOUR_JWT_TOKEN"

# Get invitation details:

curl -X GET http://localhost:3000/api/v1/invitations/INVITATION_TOKEN \
  -H "Authorization: Bearer YOUR_JWT_TOKEN"

# Accept invitation:

curl -X POST http://localhost:3000/api/v1/invitations/accept \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -d '{
    "token": "INVITATION_TOKEN"
  }'


# Update cluster status

curl -X POST http://localhost:3000/clusters/0abec7b7-bb31-4f5b-adca-c36a21a82a38/status \
  -H "X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiIwYWJlYzdiNy1iYjMxLTRmNWItYWRjYS1jMzZhMjFhODJhMzgiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xN1QwODo1NzoyOC4yNzM1Nzk1MDFaIn0.yI4qz3f-tKLepG98GSyKfWGgc1zzGZ3Fzr6NfPks7lM"

# Update cluster kubeconfig

curl -X POST \
  'http://localhost:3000/clusters/c4170bef-026f-4580-a18e-696ce0df5a03/kubeconfig' \
  -H 'X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiJjNDE3MGJlZi0wMjZmLTQ1ODAtYTE4ZS02OTZjZTBkZjVhMDMiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xMlQwODozMjoxMS45MjgwMDg4NzlaIn0.o8icfY8pGiQGbrwBY3Q-k6Ey74hbgq5ENBLqif7V-vg' \
  --data-binary '@/Users/lakshminp/kind.yaml'

# Get kubeconfig

curl -X GET \
  'http://localhost:3000/clusters/c4170bef-026f-4580-a18e-696ce0df5a03/get-kubeconfig' \
  -H 'X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiJjNDE3MGJlZi0wMjZmLTQ1ODAtYTE4ZS02OTZjZTBkZjVhMDMiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xMlQxNDoxNTo0MS42ODA2MjA3OTlaIn0.y8-HhAXDmRoOkKTa6qZKshduy3PCBmA3kIQMopE1aQc'

# Update node info

curl -X POST \
  'http://localhost:3000/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6/nodeinfo' \
  -H 'X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiJmNDlmNWM4Mi05ZGUwLTQxMTQtYWVkNi01ODkzZmViYzc5YjYiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xMVQxNjo0NjoyNS4zMDY4MzMyOThaIn0=.ZxKLjVXVnddJURAomIrhog8eHVHNryK62/p6NXjFb88=' \
  -H 'Content-Type: application/json' \
  -d '{
    "worker-1-a1234": "1.2.3.4",
    "worker-2-b5678": "5.6.7.8"
  }'

# Get infra tfvars

curl -X GET \
  'http://localhost:3000/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6/infra-tfvars' \
  -H 'X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiJmNDlmNWM4Mi05ZGUwLTQxMTQtYWVkNi01ODkzZmViYzc5YjYiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xMVQxNzowNDo0Ny4xODQ1MTU2MjZaIn0=.paenVOvpO9HDX4GG0cc0L0pEPmSmb7Ft4gzLEsemJlY='


# Get cluster tfvars

curl -X GET \
  'http://localhost:3000/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6/cluster-tfvars' \
  -H 'X-SB-Signature: eyJjbHVzdGVyX3V1aWQiOiJmNDlmNWM4Mi05ZGUwLTQxMTQtYWVkNi01ODkzZmViYzc5YjYiLCJleHBpcmVzX2F0IjoiMjAyNS0wMi0xMVQyMzoyNzowMC42NDMxNDY0NzFaIn0=.yiNkfgKWF3bUoxEQIZ36/kcRmKDuu6xTC2gp4hAMoT0='


# Scale up a cluster (add nodes)
curl -X POST http://localhost:3000/api/v1/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6/scale \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "nodes": [
      {
        "name": "worker-2",
        "size": "s-2vcpu-4gb"
      }
    ]
  }'

# Scale down a cluster (remove nodes)
curl -X POST http://localhost:3000/api/v1/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6/scale \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "deleted": ["462809f0-d76d-45cf-870d-2b299c9ddc85"]
  }'

# Delete a cluster
curl -X DELETE http://localhost:3000/api/v1/clusters/f49f5c82-9de0-4114-aed6-5893febc79b6 \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"


# Create a new project

curl -X POST 'http://localhost:3000/api/v1/projects' \
-H "Content-Type: application/json" \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{
    "name": "My Test Project",
    "description": "A test project for development",
    "cluster_id": "a4195100-b969-4c8a-9e63-f538fbee51fc"
}'


# Get all projects

curl -X GET 'http://localhost:3000/api/v1/projects' \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Delete a project

curl -X DELETE 'http://localhost:3000/api/v1/projects/a4195100-b969-4c8a-9e63-f538fbee51fc' \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Create a new app
curl -X POST http://localhost:3000/api/v1/apps \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "Content-Type: application/json" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{
  "name": "flask",
  "description": "My first application",
  "project_id": "7cf794ee-1dc5-4a23-97fa-704405353754",
  "git_repo": "https://github.com/badri/flask.git",
  "git_branch": "main",
  "is_private": false,
  "build_type": "buildpack",
  "builder_image": "paketobuildpacks/builder-jammy-full:0.3.440"
}'

# List apps
curl -X GET http://localhost:3000/api/v1/apps \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID"

# Create env vars

curl -X PATCH 'http://localhost:3000/api/v1/apps/6782948c-82d1-4553-bf79-e3bc7f960915/env-vars' \
-H "Content-Type: application/json" \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{
  "env_vars": [
    {
      "key": "DATABASE_URL",
      "value": "postgres://user:pass@host:5432/db"
    },
    {
      "key": "API_KEY",
      "value": "secret123"
    }
  ]
}'

# Update env vars

curl -X PATCH 'http://localhost:3000/api/v1/apps/6782948c-82d1-4553-bf79-e3bc7f960915/env-vars' \
-H "Content-Type: application/json" \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{
  "env_vars": [
    {
      "uuid": "ea76e340-9a26-4f06-bfeb-9a0ed18c6421",
      "key": "DATABASE_URL",
      "value": "postgres://user:pass@host:5432/db"
    },
    {
      "uuid": "6659f362-03fc-4755-95cb-8493474d858d",
      "key": "API_KEY",
      "value": "secret123444"
    }
  ]
}'

# Delete env vars

curl -X PATCH 'http://localhost:3000/api/v1/apps/6782948c-82d1-4553-bf79-e3bc7f960915/env-vars' \
-H "Content-Type: application/json" \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{
  "delete": ["API_KEY"]
}'



# Create a new app build

curl -X POST http://localhost:3000/api/v1/apps/4ae60f82-dd26-4e93-9f30-49f79a81cbd2/builds \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "Content-Type: application/json" \
-H "X-Team-UUID: $TEAM_UUID" \
-d '{}'

# List app builds

curl -X GET http://localhost:3000/api/v1/apps/4ae60f82-dd26-4e93-9f30-49f79a81cbd2/builds \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Get a specific app build

curl -X GET http://localhost:3000/api/v1/apps/4ae60f82-dd26-4e93-9f30-49f79a81cbd2/builds/4ae60f82-dd26-4e93-9f30-49f79a81cbd2 \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Stream app build logs

# Delete an app

curl -X DELETE http://localhost:3000/api/v1/apps/4ae60f82-dd26-4e93-9f30-49f79a81cbd2 \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Password reset

curl -X POST http://localhost:3000/api/v1/auth/forgot-password \
  -H "Content-Type: application/json" \
  -d '{
    "email": "user@example.com"
  }'


curl -X POST http://localhost:3000/api/v1/auth/reset-password \
  -H "Content-Type: application/json" \
  -d '{
    "token": "b27DMx47dNLSx7fSmY4tTYaQnElrLApoQzCh4EgYMaI",
    "new_password1": "password123456",
    "new_password2": "password123456"
  }'

# Create a new service

curl -X POST http://localhost:3000/api/v1/services \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "name": "my-service",
    "type": "postgresql",
    "project_id": "5544ea77-4bfa-462f-a048-0b5362d09b4e"
  }'

# Update build status

curl -X PATCH http://localhost:3000/builds/5dbbcb97-5d45-4724-add0-d9b4e144d7f8/status \
  -H "X-Cluster-Key: bw_d42e59432e294a229ec5152c62a7d09c" \
  -H "Content-Type: application/json" \
  -d '{
    "build_status": "success",
    "build_logs": "Build completed successfully"
  }'


# Get app logs token

curl -X GET http://localhost:3000/api/v1/apps/1ca2f6ab-567e-4d91-b45a-d5a2d34f9a20/logs/token \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Get app shell token

curl -X GET http://localhost:3000/api/v1/apps/1ca2f6ab-567e-4d91-b45a-d5a2d34f9a20/shell/token \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# Get service shell token

curl -X GET http://localhost:3000/api/v1/services/fa84abb3-14fd-4ecb-81b2-8377a3d3258a/shell/token \
-H "Authorization: Bearer $JWT_TOKEN" \
-H "X-Team-UUID: $TEAM_UUID"

# App volumes

curl -X POST http://localhost:3000/api/v1/apps/ee3a9b25-19bd-4979-9e3d-be0ed1c18027/volumes \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '[
    {"name": "data", "path": "/data", "size": "1Gi"}
  ]'


curl -X POST http://localhost:3000/api/v1/apps/ee3a9b25-19bd-4979-9e3d-be0ed1c18027/volumes \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '[
    {"uuid": "52c2865f-b126-4a83-8884-edf161692634", "name": "data", "path": "/data", "size": "1Gi"},
    {"name": "data2", "path": "/data2", "size": "1Gi"}
  ]'

# Delete

curl -X POST http://localhost:3000/api/v1/apps/ee3a9b25-19bd-4979-9e3d-be0ed1c18027/volumes \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "volumes": [
      {"uuid": "52c2865f-b126-4a83-8884-edf161692634", "name": "data", "path": "/data", "size": "1Gi"},
      {"name": "data3", "path": "/data3", "size": "1Gi"}
    ],
    "delete": ["f3c8138f-5f3e-437b-934b-d286c3aa583f", "e24601fa-bc73-4c8a-a497-d80ee31240e2"]
  }'

# Update app

# Update replicas
curl -X PATCH http://localhost:3000/api/v1/apps/<uuid> \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{"replicas": 3}'

# Update port
curl -X PATCH http://localhost:3000/api/v1/apps/<uuid> \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{"port": 8080}'

# Update both
curl -X PATCH http://localhost:3000/api/v1/apps/<uuid> \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{"replicas": 3, "port": 8080}'

# Get app pvt key

curl -X GET http://localhost:3000/apps/63446a1f-caaa-4d46-8357-2c4e5d502a33/ssh-key \
  -H "X-Cluster-Key: bw_d42e59432e294a229ec5152c62a7d09c"


curl -H "Authorization: token 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" \
     -H "Accept: application/vnd.github.v3+json" \
     "https://api.github.com/repos/Concinnity-Tech/shusrut/commits/master"


curl -X GET https://api.kubenest.io/apps/136fabcd-8b7f-45dd-9d8f-23490dff85ea/ssh-key \
  -H "X-Cluster-Key: bw_ee61004127672b55e45705ab16430d"

# Build variables

curl -X PATCH 'http://localhost:3000/api/v1/apps/ee3a9b25-19bd-4979-9e3d-be0ed1c18027/build-vars' \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "build_vars": [
      {
        "key": "NODE_ENV",
        "value": "production"
      },
      {
        "key": "BUILD_FLAG",
        "value": "--production"
      }
    ]
  }'

# Cancel build

e1fafb09-5c6b-472e-a331-bcccd2847b7a

curl -X DELETE 'http://localhost:3000/api/v1/apps/1ca2f6ab-567e-4d91-b45a-d5a2d34f9a20/builds/e1fafb09-5c6b-472e-a331-bcccd2847b7a' \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" | jq .


# RBAC

# Stacks

curl -X GET \
  "http://localhost:3000/api/v1/stacks" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" | jq .


curl -X POST "http://localhost:3000/api/v1/stacks" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @wordpress-stack-updated.json

# single component stack
curl -X POST "http://localhost:3000/api/v1/stacks" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @sample-component-stack.json


# multiple component stack
curl -X POST "http://localhost:3000/api/v1/stacks" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @flask-pg-stack.json


curl -X POST "http://localhost:3000/api/v1/stacks/associate" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "cluster_uuid": "221266d3-ba2a-4f35-90d2-5e4bb114fa6a",
    "stack_uuid": "ac1bf3b9-9665-45b5-8c21-18917f5b720d"
  }'

# Stackdeploys

curl -X POST http://localhost:3000/api/v1/stackdeploys \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @wordpress-stackdeploy.json

curl -X PATCH http://localhost:3000/api/v1/stackdeploys/454cd3b3-ecf5-4c96-9bc1-23f02579429a \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "parameter_values": {
      "wordpressUsername": "admin",
      "wordpressPassword": "new-password"
    },
    "additional_values": {
      "service": {
        "type": "NodePort"
      }
    }
  }' | jq .

# Single component stack deploy

curl -X POST http://localhost:3000/api/v1/stackdeploys \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @flask-stackdeploy.json

# Single component stack deploy patch

curl -X PATCH http://localhost:3000/api/v1/stackdeploys/b20f8530-7042-4a1b-9e61-ecef5eaa7658 \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d '{
    "components": [
    {
      "name": "web-app",
      "git_ref": "afeb77ebcf2aa10777878300cd0ec3d063eb3f6f"
    }
    ]
  }' | jq .

# multiple component stack deploy

curl -X POST http://localhost:3000/api/v1/stackdeploys \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @flask-pg-stackdeploy.json


curl -X POST "http://localhost:3000/api/v1/stacks" \
  -H "Authorization: Bearer $JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Team-UUID: $TEAM_UUID" \
  -d @alfresco-stack.json

