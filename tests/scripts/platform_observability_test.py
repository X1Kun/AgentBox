"""Monitoring ownership, credential separation and missing-data contracts."""
from contextlib import nullcontext
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'scripts'))
from lib.platform_batch import BatchError
from lib.platform_manifests import read_yaml
from lib.platform_observability import Observability, OWNER


class ObservationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.lab = Mock(root=self.root, config=ROOT / 'deploy', tools={'docker': 'docker'})
        self.lab.operation_lock.side_effect = lambda: nullcontext()
        self.stack = Observability(self.lab)

    def test_foreign_containers_and_volumes_are_never_adopted(self):
        self.lab.runner.run.side_effect = ['container-id', json.dumps({OWNER: 'someone-else'})]
        with self.assertRaisesRegex(BatchError, 'another project'):
            self.stack._owned_containers('owner')
        self.lab.runner.run.side_effect = ['', 'volume-name', json.dumps({OWNER: 'someone-else'})]
        with self.assertRaisesRegex(BatchError, 'another session'):
            self.stack._owned_containers('owner')

    def test_repeated_up_preserves_running_observer_and_rbac_identities(self):
        state = {'owner': 'owner', 'docker_id': 'daemon', 'observer': {'pid': 123, 'start_ticks': 'time'},
                 'rbac': [{'kind': 'Role', 'name': 'agentbox-observer', 'uid': 'original'}]}
        self.stack.directory.mkdir(parents=True)
        self.stack.state_path.write_text('{}')
        self.lab.owned.return_value = ({'session': 'owner', 'docker_id': 'daemon'}, [])
        images = [v['image'] for v in self.stack.lock['images'].values()]
        self.lab.runner.run.return_value = json.dumps([{'RepoDigests': images}])
        with patch.object(self.stack, 'state', return_value=state), patch.object(self.stack, '_owned_containers', return_value=['owned']), \
                patch.object(self.stack, 'http', return_value=b'{"dashboard":{"uid":"agentbox-observation"}}'):
            self.stack.up()
        written = self.lab.write_json.call_args_list[0].args[1]
        self.assertEqual(written['observer'], state['observer'])
        self.assertEqual(written['rbac'], state['rbac'])

    def test_missing_or_wrong_namespace_scope_does_not_stop_existing_observer(self):
        for scope in ({}, {'session': 'default'}, {'other': 'agentbox-test'},
                      {'session': 'agentbox-test', 'batch': 'agentbox-test'}):
            with self.subTest(scope=scope), patch.object(self.stack, 'state', return_value={'owner': 'owner'}), \
                    patch.object(self.stack, 'stop_observer') as stop:
                with self.assertRaises(BatchError):
                    self.stack.watch(scope)
                stop.assert_not_called()
        self.lab.kubectl.return_value = json.dumps({'metadata': {'labels': {OWNER: 'someone-else'}}})
        with patch.object(self.stack, 'state', return_value={'owner': 'owner'}), patch.object(self.stack, 'stop_observer') as stop:
            with self.assertRaises(BatchError):
                self.stack.watch({'session': 'agentbox-test'})
            stop.assert_not_called()
        self.assertTrue(all(c.args[:2] == ('get', 'namespace') for c in self.lab.kubectl.call_args_list))

    def test_reused_pid_is_not_signalled(self):
        state = {'observer': {'pid': 123, 'start_ticks': 'old'}}
        with patch.object(self.stack, 'state', return_value=state), patch.object(Path, 'exists', return_value=True), \
                patch.object(Path, 'read_bytes', return_value=b'unrelated\0'), patch.object(self.stack, 'ticks', return_value='new'), \
                patch('lib.platform_observability.os.kill') as kill:
            with self.assertRaisesRegex(BatchError, 'replaced process'):
                self.stack.stop_observer()
            kill.assert_not_called()

    def test_target_validation_preserves_previous_good_configuration(self):
        self.stack.render('owner')
        self.stack.targets({'session': '127.0.0.1:9091'})
        path = self.stack.config / 'prometheus/targets.yaml'
        previous = path.read_bytes()
        for address in ('localhost:9091', '0.0.0.0:9091', '127.0.0.1:9091/path', '127.0.0.1:0', '127.0.0.1:65536'):
            with self.subTest(address=address), self.assertRaises(BatchError):
                self.stack.targets({'session': address})
            self.assertEqual(path.read_bytes(), previous)

    def test_private_credentials_stay_outside_generated_dashboard_and_targets(self):
        self.stack.render('owner')
        password = self.stack.directory / 'grafana-password'
        self.assertEqual(password.stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.stack.directory / 'private.env').stat().st_mode & 0o777, 0o600)
        secret = password.read_text().strip()
        for path in self.stack.config.rglob('*'):
            if path.is_file():
                self.assertNotIn(secret, path.read_text())
        self.assertEqual(json.loads((self.stack.config / 'grafana/dashboards/agentbox.json').read_text()),
                         read_yaml(self.stack.source / 'dashboard.yaml'))

    def test_collectors_and_dashboard_keep_unknown_and_host_scopes_explicit(self):
        role = read_yaml(self.stack.source / 'namespace-role.yaml')
        self.assertTrue(all(set(rule['verbs']) <= {'get', 'list', 'watch'} and 'secrets' not in rule['resources'] for rule in role['rules']))
        compose = read_yaml(self.stack.source / 'compose.yaml')
        lock = read_yaml(self.stack.source / 'versions.lock.yaml')
        for name, service in compose['services'].items():
            self.assertIn(service['image'], {v['image'] for v in lock['images'].values()})
            self.assertIn('@sha256:', service['image'])
            self.assertEqual(service['cap_drop'], ['ALL'])
        self.assertEqual(compose['services']['host']['volumes'], ['/proc:/host/proc:ro', '/sys:/host/sys:ro'])
        dashboard = read_yaml(self.stack.source / 'dashboard.yaml')
        for panel in dashboard['panels']:
            for target in panel['targets']:
                self.assertNotIn('or vector(0)', target['expr'])
                if 'node_pressure_' in target['expr']:
                    self.assertNotIn('agentbox_runtime_resource', target['expr'])


if __name__ == '__main__':
    unittest.main()
