import importlib.util
import pathlib
import re
import unittest


PATH = pathlib.Path(__file__).with_name('check-oss-hygiene.py')
SPEC = importlib.util.spec_from_file_location('oss_hygiene', PATH)
HYGIENE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HYGIENE)


class PrivateAddressTests(unittest.TestCase):
    def test_private_addresses_remain_detected_without_flagging_accessibility_rule_ids(self):
        pattern = dict(HYGIENE.PATTERNS)['private network address']
        for address in ('10.0.0.1', '10.255.255.255', '192.168.0.1', '172.16.0.1', '172.31.255.255'):
            with self.subTest(address=address):
                self.assertIsNotNone(re.search(pattern, f'https://{address}:8080/private'))
        for public in ('172.15.0.1', '172.32.0.1', '192.169.0.1', 'RGAA-10.8.1', 'RGAA-10.6.1', 'RGAA-10.4.2'):
            with self.subTest(public=public):
                self.assertIsNone(re.search(pattern, public))


if __name__ == '__main__':
    unittest.main()
