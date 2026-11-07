package backend

import "maps"

// offlineCapabilities are the capability facts the host plans the spanner
// engine with when no session states the server's (engine.default-capabilities,
// contract 25.22): an offline render, a generation without a database, an
// export, and `ptah sql lint`, whose CAP001 finding for CREATE INDEX
// CONCURRENTLY reads create_index_concurrently. Release line "0" states the
// same facts, for a --server-version that selects it. A present false is an
// established answer; the list names every key the host's vocabulary holds.
//
// They are the facts of the host's SpannerPostgres preset, which Ptah planned
// Spanner with while it built in a Spanner engine.
// testdata/offline-capabilities.golden.json holds the preset as host
// 49e1d61ec answered for the spanner dialect, recorded by
// testdata/capabilityoracle, and a test compares the two. The host's
// capability probe at that commit decides 117 of the facts against the
// emulator behind PGAdapter v0.56.2, and each answers as stated here; it
// leaves the other 32 undecided on that endpoint.
//
// A session states fewer facts (sessionCapabilities), the ones the provider
// plans with, and each agrees with this list. A host that does not list the
// feature receives none of them.
func offlineCapabilities() map[string]bool {
	return map[string]bool{
		"add_column_with_default": true, "alter_column_default": true, "alter_column_drop_not_null": true,
		"alter_column_set_not_null": true, "alter_column_type": true, "check_constraints": true,
		"check_constraints_enforced": true, "common_table_expressions": true, "correlated_subqueries": true,
		"create_index_statement": true, "create_or_replace_view": true, "drop_constraint_generic": true,
		"drop_index_if_exists": true, "expression_defaults": true, "foreign_keys": true,
		"foreign_keys_create_backing_index": true, "generated_columns": true, "index_covering_columns": true,
		"non_equi_joins": true, "object_existence_guards": true, "offset_without_limit": true, "rename_table": true,
		"returning_clause": true, "row_deletion_policy": true, "sequence_start_counter_only": true, "sequences": true,
		"serial_columns": true, "unique_index_on_existing_table": true, "views": true,

		"add_constraint_not_valid": false, "advisory_locks": false, "alter_generated_column_expression": false,
		"alter_table_algorithm_lock": false, "async_indexes": false, "async_replication": false,
		"backup_collections": false, "catalog_check_constraint_table_name": false, "catalog_default_privileges": false,
		"catalog_dependencies": false, "catalog_partitions": false, "catalog_recursive_cte": false,
		"catalog_row_statistics": false, "catalog_trigger_definitions": false, "catalog_vector_info": false,
		"catalog_view_dependencies": false, "changefeed_schema_changes": false, "changefeed_topic_auto_partitioning": false,
		"changefeed_user_sids": false, "changefeeds": false, "check_grant_statement": false, "column_families": false,
		"column_family_cache_mode": false, "column_store_tables": false, "comment_attributes": false,
		"composite_types": false, "constraint_comments": false, "continuous_aggregates": false, "coordination_nodes": false,
		"create_index_concurrently": false, "create_or_replace_trigger": false, "database_grants": false,
		"ddl_inside_transaction": false, "deferrable_constraints": false, "deferrable_keys": false,
		"document_type_defaults": false, "domain_comments": false, "domain_types": false, "drop_check_clause": false,
		"drop_constraint_if_exists": false, "drop_index_concurrently": false, "enum_custom_type": false,
		"enum_inline_column": false, "extension_comments": false, "external_data_source_secret_paths": false,
		"external_data_sources": false, "external_object_replace": false, "foreign_key_delete_column_list": false,
		"foreign_key_match_full": false, "foreign_key_match_partial": false,
		"foreign_keys_require_indexed_reference": false, "foreign_keys_require_unique_reference": false,
		"full_text_indexes": false, "function_comments": false, "functions": false, "group_principals": false,
		"hypertables": false, "index_comments": false, "index_include_spgist": false, "index_partitioning": false,
		"index_rename": false, "invisible_indexes": false, "key_bloom_filter": false, "local_bloom_indexes": false,
		"local_min_max_indexes": false, "local_ngram_indexes": false, "materialized_view_comments": false,
		"materialized_view_options": false, "materialized_views": false, "migration_lock_timeout": false,
		"migration_statement_timeout": false, "named_not_null_constraints": false, "not_enforced_checks": false,
		"not_enforced_foreign_keys": false, "parameterized_decimal": false, "partitioning_options": false,
		"policy_comments": false, "postgres_catalog_functions": false, "primary_key_alterable": false,
		"primary_key_required": false, "procedure_comments": false, "procedures": false, "range_types": false,
		"read_replicas": false, "relative_grant_paths": false, "rename_column_clause": false,
		"replication_secret_paths": false, "resource_pools": false, "role_comments": false, "role_management": false,
		"role_membership": false, "routine_replacement_resets_definer": false, "row_deletion_policy_epoch_column": false,
		"row_level_security": false, "row_level_ttl": false, "schema_comments": false, "secrets": false,
		"sequence_comments": false, "serial_sequence_keeps_range": false, "serial_sequence_options": false,
		"show_routine_privilege": false, "small_integer_defaults": false, "streaming_queries": false, "tiered_ttl": false,
		"topic_consumer_availability_period": false, "topics": false, "transactional_ddl": false, "transfers": false,
		"trigger_comments": false, "triggers": false, "type_comments": false, "unique_constraints": false,
		"unique_nulls_distinct_clause": false, "vector_bit_type": false, "vector_index_maintained_on_write": false,
		"vector_indexes": false, "view_comments": false, "view_options": false, "wide_date_time_types": false,
		"xml_type": false,
	}
}

// lineCapabilities are the facts of release line "0", the only line: the
// offline facts, since the managed service has no other line to differ on.
func lineCapabilities() map[string]bool {
	return maps.Clone(offlineCapabilities())
}
