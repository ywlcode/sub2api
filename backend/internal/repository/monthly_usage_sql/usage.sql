WITH usage_buckets AS (
    SELECT (l.created_at AT TIME ZONE 'Asia/Shanghai')::date AS usage_day,
           l.user_id,
           COALESCE(NULLIF(btrim(l.requested_model), ''), l.model) AS model,
           -- Unicode White_Space, independent of the database locale's POSIX space class.
           NULLIF(lower(btrim(l.platform,
               U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')), '') AS platform,
           l.group_id, GROUPING(l.group_id) = 1 AS by_platform,
           count(*) AS requests,
           sum(l.input_tokens) AS input_tokens,
           sum(l.output_tokens) AS output_tokens,
           sum(l.cache_read_tokens) AS cache_read_tokens,
           sum(l.input_tokens::bigint + l.output_tokens + l.cache_creation_tokens + l.cache_read_tokens) AS total_tokens,
           sum(l.actual_cost) AS actual_cost
    FROM usage_logs l
    WHERE l.created_at >= ($1::date::timestamp AT TIME ZONE 'Asia/Shanghai')
      AND l.created_at < (($1::date + interval '1 month') AT TIME ZONE 'Asia/Shanghai')
      AND NOT EXISTS (SELECT 1 FROM users a WHERE a.id = l.user_id AND a.role = 'admin')
    -- Each log contributes once per platform (including NULL) and once per used group.
    GROUP BY 1, 2, 3, GROUPING SETS ((4), (l.group_id))
), daily_usage AS (
    SELECT usage_day, user_id, model,
           array_agg(group_id) FILTER (WHERE group_id IS NOT NULL) AS group_ids,
           sum(requests) FILTER (WHERE by_platform) AS requests,
           sum(input_tokens) FILTER (WHERE by_platform) AS input_tokens,
           sum(output_tokens) FILTER (WHERE by_platform) AS output_tokens,
           sum(cache_read_tokens) FILTER (WHERE by_platform) AS cache_read_tokens,
           sum(total_tokens) FILTER (WHERE by_platform) AS total_tokens,
           sum(actual_cost) FILTER (WHERE by_platform) AS actual_cost,
           json_object_agg(platform, json_build_array(requests::text, input_tokens::text, output_tokens::text,
                           cache_read_tokens::text, total_tokens::text, actual_cost::text))
               FILTER (WHERE platform IS NOT NULL) AS platform_usage
    FROM usage_buckets
    GROUP BY usage_day, user_id, model
), group_names AS (
    SELECT id, NULLIF(name, '')::text COLLATE "C" AS name FROM groups
), departments AS (
    SELECT DISTINCT ON (v.user_id) v.user_id,
           COALESCE(CASE WHEN k.type = 'select' THEN (
               SELECT btrim(opt->>'label')
               FROM jsonb_array_elements(CASE WHEN jsonb_typeof(k.options) = 'array' THEN k.options END) AS opt
               WHERE opt->>'value' = v.value AND opt->>'label' ~ '[^[:space:]]'
               LIMIT 1
           ) END, btrim(v.value)) AS department
    FROM user_attribute_definitions k
    JOIN user_attribute_values v ON v.attribute_id = k.id
    CROSS JOIN (SELECT $2::bigint AS department_attribute_id) cfg
    WHERE k.deleted_at IS NULL AND v.value ~ '[^[:space:]]'
      AND CASE WHEN cfg.department_attribute_id = 0 THEN k.key IN ('dingtalk_department', 'department')
               ELSE k.id = cfg.department_attribute_id END
    ORDER BY v.user_id, (k.key = 'dingtalk_department') DESC
), profile_names AS (
    SELECT v.user_id, NULLIF(btrim(v.value), '') AS profile_name
    FROM user_attribute_definitions k
    JOIN user_attribute_values v ON v.attribute_id = k.id
    WHERE k.deleted_at IS NULL AND k.key = 'dingtalk_name'
)
SELECT to_char(d.usage_day, 'YYYY-MM-DD') AS usage_day, d.user_id,
       CASE WHEN u.username !~ $3 THEN u.username END AS username,
       CASE WHEN p.profile_name !~ $3 THEN p.profile_name END AS profile_name,
       dept.department,
       (SELECT string_agg(DISTINCT g.name, ', ' ORDER BY g.name)
        FROM group_names g WHERE g.id = ANY(d.group_ids)) AS used_groups,
       d.model, d.requests, d.input_tokens, d.output_tokens, d.cache_read_tokens,
       d.total_tokens, d.actual_cost, d.platform_usage
FROM daily_usage d
LEFT JOIN users u ON u.id = d.user_id
LEFT JOIN profile_names p ON p.user_id = d.user_id
LEFT JOIN departments dept ON dept.user_id = d.user_id
ORDER BY d.usage_day, d.user_id, d.model COLLATE "C"
