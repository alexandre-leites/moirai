CREATE OR REPLACE FUNCTION app.notify_dashboard_event() RETURNS trigger AS $$
BEGIN
    IF TG_TABLE_NAME = 'workflow_events' THEN
        PERFORM pg_notify(
            'moirai_dashboard_events',
            json_build_object(
                'id', NEW.id,
                'event_type', 'workflow',
                'workflow_run_id', NEW.workflow_run_id
            )::text
        );
    ELSE
        PERFORM pg_notify(
            'moirai_dashboard_events',
            json_build_object(
                'id', 'runner:' || NEW.id || ':' || extract(epoch FROM clock_timestamp())::bigint,
                'event_type', 'runner',
                'runner', json_build_object(
                    'id', NEW.id,
                    'name', NEW.name,
                    'enabled', NEW.enabled,
                    'draining', NEW.draining,
                    'status', NEW.status,
                    'labels', NEW.labels,
                    'last_seen_at', NEW.last_seen_at
                )
            )::text
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER workflow_events_dashboard_notify
AFTER INSERT ON app.workflow_events
FOR EACH ROW EXECUTE FUNCTION app.notify_dashboard_event();

CREATE TRIGGER runners_dashboard_notify
AFTER INSERT OR UPDATE OF enabled, draining, status, labels, last_seen_at ON app.runners
FOR EACH ROW EXECUTE FUNCTION app.notify_dashboard_event();
