/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bootstrap

// OTel collector configuration presets ported from the rossoctl-deps Helm chart
// (charts/rossoctl-deps/values.yaml). The assembleCollectorConfig function merges
// these in the same order as the Helm rossoctl.otel.collectorConfig helper.

const baseConfig = `
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318
exporters:
  debug:
    verbosity: detailed
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 1000
  batch: {}
extensions:
  health_check: {}
service:
  extensions: [health_check]
  pipelines: {}
`

const defaultPreset = `
service:
  pipelines:
    traces/default:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [debug]
`

const phoenixPreset = `
exporters:
  otlp/phoenix:
    endpoint: phoenix:4317
    tls:
      insecure: true
processors:
  filter/phoenix:
    traces:
      span:
        - 'IsMatch(name, "^a2a\\..*")'
        - 'attributes["http.method"] != nil'
        - 'attributes["mcp.method.name"] == "initialize"'
        - 'attributes["mcp.method.name"] == "notifications/initialized"'
        - 'attributes["mcp.method.name"] == "notifications/cancelled"'
        - 'attributes["mcp.method.name"] == "tools/list"'
  transform/genai_to_openinference:
    trace_statements:
      - context: span
        statements:
          - >-
            set(attributes["llm.model_name"], attributes["gen_ai.request.model"])
            where attributes["gen_ai.request.model"] != nil
          - >-
            set(attributes["llm.model_name"], attributes["gen_ai.response.model"])
            where attributes["gen_ai.response.model"] != nil and attributes["llm.model_name"] == nil
          - >-
            set(attributes["llm.token_count.prompt"], attributes["gen_ai.usage.input_tokens"])
            where attributes["gen_ai.usage.input_tokens"] != nil
          - >-
            set(attributes["llm.token_count.completion"], attributes["gen_ai.usage.output_tokens"])
            where attributes["gen_ai.usage.output_tokens"] != nil
          - >-
            set(attributes["llm.token_count.total"],
            attributes["gen_ai.usage.input_tokens"] + attributes["gen_ai.usage.output_tokens"])
            where attributes["gen_ai.usage.input_tokens"] != nil
            and attributes["gen_ai.usage.output_tokens"] != nil
          - >-
            set(attributes["llm.provider"], attributes["gen_ai.system"])
            where attributes["gen_ai.system"] != nil
          - >-
            set(attributes["llm.system"], attributes["gen_ai.system"])
            where attributes["gen_ai.system"] != nil
          - >-
            set(attributes["llm.invocation_parameters"],
            Concat(["{\"temperature\":", Concat([attributes["gen_ai.request.temperature"], "}"], "")], ""))
            where attributes["gen_ai.request.temperature"] != nil
service:
  pipelines:
    traces/phoenix:
      receivers: [otlp]
      processors: [memory_limiter, filter/phoenix, transform/genai_to_openinference, batch]
      exporters: [otlp/phoenix]
`

const mlflowPreset = `
exporters:
  otlphttp/mlflow:
    traces_endpoint: http://mlflow:5000/v1/traces
    tls:
      insecure: true
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s
      max_elapsed_time: 300s
    sending_queue:
      enabled: true
      num_consumers: 2
      queue_size: 1000
processors:
  filter/mlflow:
    traces:
      span:
        - 'IsMatch(name, "^a2a\\..*")'
        - 'attributes["http.method"] != nil and IsMatch(name, "^(POST|GET|DELETE)$")'
        - 'IsMatch(name, "^mcp-router\\..*") and (attributes["http.method"] == "GET" or attributes["http.method"] == "DELETE")'
        - 'attributes["mcp.method.name"] == "initialize"'
        - 'attributes["mcp.method.name"] == "notifications/initialized"'
        - 'attributes["mcp.method.name"] == "notifications/cancelled"'
        - 'attributes["mcp.method.name"] == "tools/list"'
service:
  pipelines:
    traces/mlflow:
      receivers: [otlp]
      processors: [memory_limiter, filter/mlflow, batch]
      exporters: [debug, otlphttp/mlflow]
`

const mlflowAuthPreset = `
extensions:
  oauth2client/mlflow:
    client_id: ${env:MLFLOW_CLIENT_ID}
    client_secret: ${env:MLFLOW_CLIENT_SECRET}
    token_url: ${env:KEYCLOAK_TOKEN_URL}
    scopes: ["openid"]
    timeout: 10s
service:
  extensions: [health_check, oauth2client/mlflow]
`

const rhoaiMlflowAuthPreset = `
exporters:
  otlphttp/mlflow:
    compression: none
    retry_on_failure:
      enabled: false
extensions:
  bearertokenauth/mlflow:
    filename: /var/run/secrets/kubernetes.io/serviceaccount/token
service:
  extensions: [health_check, bearertokenauth/mlflow]
`
