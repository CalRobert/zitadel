SELECT 
    instance_id
    , event_date
FROM 
    projections.current_states
WHERE
    instance_id = $1
    AND projection_name = $2
FOR UPDATE;