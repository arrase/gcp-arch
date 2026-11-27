# Synthetic GCP Architecture Catalog (`testdata/catalog/`)

Realistic synthetic `FactsFile` (`schema_version: "v3"`) snapshots injected at the Stage 1 (`internal/extract.LoadFacts`) boundary covering five architectural complexity categories, plus deterministic primary/transversal resource and functional-edge conservation inventories in [`manifest.json`](./manifest.json).

## Fixtures

| # | Fixture | Category | Projects | Resources (Primary / Transversal) | Edges (Functional) | Topology & Adversarial Features |
|---:|---|---|---|---:|---:|---|
| 0 | [`../acme_cicd_v3.facts.json`](../acme_cicd_v3.facts.json) | Baseline | `acme-cicd` + PSA + 2 OOS | 44 (39 / 5) | 93 (64) | Cloud Build triggers, GKE Atlantis/SonarQube, Cloud SQL PSA, Workload Identity |
| 1 | [`cat1_cloudrun_cloudsql.facts.json`](./cat1_cloudrun_cloudsql.facts.json) | Cat 1 | `shop-api-prod` + PSA | 14 (9 / 5) | 16 (8) | GLB -> Cloud Run -> Serverless VPC Connector -> Cloud SQL (`POSTGRES_16`, PSA) + GCS + DB Migration Job |
| 2 | [`cat1_gce_mig_alloydb.facts.json`](./cat1_gce_mig_alloydb.facts.json) | Cat 1 | `erp-core-prod` + PSA | 15 (10 / 5) | 18 (9) | Regional LB -> GCE MIG in VPC/Subnet + Cloud Router/NAT -> AlloyDB (PSA) + Redis + GCS + Bastion + DNS |
| 3 | [`cat2_pubsub_dlq_cloudrun.facts.json`](./cat2_pubsub_dlq_cloudrun.facts.json) | Cat 2 | `orders-async-prod` | 20 (12 / 8) | 25 (10) | Cloud Scheduler -> Ingest Cloud Run -> Pub/Sub Topic/Sub (`dead_letter` -> DLQ Topic) -> Worker Cloud Run -> DLQ Replay Cloud Function -> Firestore & GCS |
| 4 | [`cat2_eventarc_workflows_retry.facts.json`](./cat2_eventarc_workflows_retry.facts.json) | Cat 2 | `media-ingest-prod` | 21 (14 / 7) | 24 (12) | GCS -> Eventarc Trigger -> Workflows -> Cloud Tasks retry queue -> Cloud Function -> Pub/Sub + DLQ -> Notification Cloud Run & Firestore |
| 5 | [`cat3_shared_vpc_hybrid_armor.facts.json`](./cat3_shared_vpc_hybrid_armor.facts.json) | Cat 3 | Org + Folder + 3 projects + PSA | 35 (26 / 9) | 39 (23) | Shared VPC Host + 2 Service Projects, GLB/ILB, GKE + Cloud Run + Connector, Cloud Router/NAT + HA VPN + Interconnect, VPC/PSA peering, Cloud Armor/Firewall/Route plumbing counts |
| 6 | [`cat3_ncc_hub_spoke_psc.facts.json`](./cat3_ncc_hub_spoke_psc.facts.json) | Cat 3 | Org + 2 Folders + 4 projects + PSA | 37 (27 / 10) | 35 (23) | Network Connectivity Center Hub (`spoke_count: 3`) bridging 2 spoke VPCs + Ops VPC + VPN/Interconnect, PSC `ServiceAttachment` -> ILB -> MIG, DNS, AlloyDB + Filestore |
| 7 | [`cat4_cdc_dataflow_bigquery_vertex.facts.json`](./cat4_cdc_dataflow_bigquery_vertex.facts.json) | Cat 4 | `data-lakehouse-prod`, `ml-serving-prod` + PSA | 32 (22 / 10) | 37 (20) | Cloud SQL + AlloyDB -> Datastream CDC -> GCS/Pub/Sub -> Streaming Dataflow -> BigQuery (`BQ_LINEAGE`) -> Vertex `FeatureOnlineStore` / `Endpoint` / `PublisherModel` |
| 8 | [`cat4_agentic_rag_discovery_vertex.facts.json`](./cat4_agentic_rag_discovery_vertex.facts.json) | Cat 4 | `ai-agents-prod`, `knowledge-etl-prod` | 31 (22 / 9) | 35 (19) | GLB -> Cloud Run Agent Gateway -> Vertex `ReasoningEngine` + `DiscoveryEngine` + `IndexEndpoint` + `PublisherModel` + Composer/Dataproc/Metastore/Dataplex -> BigQuery |
| 9 | [`cat5_high_fan_in_out_24svc.facts.json`](./cat5_high_fan_in_out_24svc.facts.json) | Cat 5 | Org + Folder + Shared VPC + 2 svc projects + PSA | 70 (52 / 18) | 221 (166) | Adversarial fan-in/fan-out: 24 primary services (18 Cloud Run + 6 GKE Deployments) with 54–57 char names sharing Pub/Sub bus, Cloud SQL, AlloyDB, Redis Cluster, BigQuery, and GCS |
| 10 | [`cat5_transversal_iam_mesh_explosion.facts.json`](./cat5_transversal_iam_mesh_explosion.facts.json) | Cat 5 | Org + Folder + 3 projects + PSA | 77 (49 / 28) | 190 (94) | Adversarial transversal density: 22 primary services (51–56 char names), 10 ServiceAccounts, 6 KMS keys, 8 Secrets, 2 LogSinks, 2 AlertPolicies, 3 NAT Routers, dense IAM/WI/CMEK/Monitoring mesh |

## Conservation & Verification

- Every fixture passes `extract.LoadFacts`, `extract.WriteFacts` byte-determinism, unique canonical AIP-122 URIs, `len(Resources) <= 500`, complete edge closure, valid `parent_uri` chains, and zero `extract.IsPlumbingAssetType` leaks in `Resources`.
- [`manifest.json`](./manifest.json) pins `expected_primary_uris`, `expected_transversal_uris`, and `expected_functional_edge_ids` (non-heuristic functional dependencies between primary resources) for deterministic conservation checks in `e2e/catalog_fixtures_test.go` and `e2e/catalog_e2e_test.go`.
- Offline fixture validation: `go test -count=1 ./e2e/...`
- Live Vertex AI + Chrome E2E & stability gate: `GCP_ARCH_E2E=1 go test -v -count=1 -timeout 20m ./e2e/...`
