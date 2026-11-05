"""Enforce executable locations and one-way script dependencies in fresh interpreters."""
import ast
import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest
import builtins
import symtable

ROOT=Path(__file__).resolve().parents[2]


class ScriptArchitecture(unittest.TestCase):
    def test_extracted_modules_have_no_unbound_global_references(self):
        allowed=set(dir(builtins))|{'__file__','__name__','__package__'}
        for path in (ROOT/'scripts/lib').rglob('*.py'):
            table=symtable.symtable(path.read_text(),str(path),'exec')
            declared={s.get_name() for s in table.get_symbols() if s.is_assigned() or s.is_imported() or s.is_namespace()}
            def visit(current):
                for symbol in current.get_symbols():
                    if symbol.is_global() and symbol.is_referenced():
                        self.assertIn(symbol.get_name(),declared|allowed,(path,symbol.get_name()))
                for child in current.get_children():visit(child)
            visit(table)

    def test_shared_environment_queries_without_scenario_imports(self):
        sys.path.insert(0,str(ROOT/'scripts'))
        from unittest.mock import Mock
        from lib.observability.environment import ObservationEnvironment
        environment=object.__new__(ObservationEnvironment)
        environment.stack=Mock()
        environment.stack.http.return_value='{"status":"success","data":{"result":[{"value":[0,"1"]}]}}'
        self.assertEqual(environment.query('up{job="observer"}'),[{'value':[0,'1']}])
        self.assertIn('query=up%7Bjob%3D%22observer%22%7D',environment.stack.http.call_args.args[1])

    def test_lower_layers_do_not_load_scenarios_or_cli(self):
        allowed={'core':{'core'},'manifests':{'core','manifests'},
                 'platform':{'core','manifests','platform'},
                 'product':{'core','manifests','platform','product'},
                 'observability':{'core','manifests','platform','product','observability'}}
        for package,dependencies in allowed.items():
            for path in (ROOT/'scripts/lib'/package).rglob('*.py'):
                for node in ast.walk(ast.parse(path.read_text())):
                    if isinstance(node,ast.ImportFrom):
                        module=node.module or ''
                        self.assertNotIn(module,{'platform_lab','prepare_batch_images','render_product','render_manifests'},path)
                        if module.startswith('lib.'):
                            self.assertIn(module.split('.')[1],dependencies,(path,module))
                    elif isinstance(node,ast.Import):
                        for alias in node.names:
                            self.assertNotIn(alias.name,{'platform_lab','prepare_batch_images'},path)
        for path in (ROOT/'scripts/lib/experiments').glob('*.py'):
            for node in ast.walk(ast.parse(path.read_text())):
                if isinstance(node,ast.ImportFrom) and (node.module or '').startswith('lib.acceptance'):
                    self.assertTrue(node.module.startswith('lib.acceptance.clients.'),(path,node.module))

    def test_payloads_are_outside_host_libraries_and_have_no_host_imports(self):
        self.assertFalse(list((ROOT/'scripts/lib').glob('*.py')))
        paths=[ROOT/'workloads/images/worker.py',*(ROOT/'tests/fixtures/probes').glob('*.py')]
        self.assertGreaterEqual(len(paths),5)
        for path in paths:
            for node in ast.walk(ast.parse(path.read_text())):
                if isinstance(node,ast.ImportFrom):
                    self.assertFalse((node.module or '').startswith('lib.'),path)
        self.assertTrue((ROOT/'scripts/lib/platform/resources/gvisor_node_processes.sh').is_file())

    def test_platform_commands_import_without_pillow_or_scenario_modules(self):
        program=r'''
import builtins,importlib.util,json,sys
from contextlib import redirect_stdout
import io
sys.path.insert(0,sys.argv[1]+'/scripts')
original=builtins.__import__
def guarded(name,*args,**kwargs):
    if name=='PIL' or name.startswith('PIL.'):
        raise AssertionError('unrelated image dependency imported')
    return original(name,*args,**kwargs)
builtins.__import__=guarded
spec=importlib.util.spec_from_file_location('entry',sys.argv[1]+'/scripts/platform_lab.py')
entry=importlib.util.module_from_spec(spec);spec.loader.exec_module(entry)
with redirect_stdout(io.StringIO()):
    assert entry.main(['preflight','--dry-run'])==0
assert not any(n.startswith(('lib.acceptance','lib.experiments')) for n in sys.modules)
'''
        result=subprocess.run([sys.executable,'-c',program,str(ROOT)],capture_output=True,text=True,timeout=15)
        self.assertEqual(result.returncode,0,result.stderr)

    def test_platform_classes_do_not_own_acceptance_flows(self):
        forbidden={'test','current_client_bundle','send_marker','inspect_supply','outsider_blocked','verify_exhaustion_rollback'}
        for path in (ROOT/'scripts/lib/platform').glob('*.py'):
            tree=ast.parse(path.read_text())
            for cls in (n for n in tree.body if isinstance(n,ast.ClassDef)):
                self.assertFalse(forbidden & {n.name for n in cls.body if isinstance(n,ast.FunctionDef)},(path,cls.name))


if __name__=='__main__':unittest.main()
