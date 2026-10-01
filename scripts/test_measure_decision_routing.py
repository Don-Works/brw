import importlib.util
import pathlib
import random
import sys
import unittest

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('routing', pathlib.Path(__file__).with_name('measure-decision-routing.py'))
routing = importlib.util.module_from_spec(spec)
spec.loader.exec_module(routing)


class RoutingTest(unittest.TestCase):
    def test_unique_support_and_ambiguity(self):
        for probabilities, expected in [({'a': 0.95, 'b': 0.1}, 'a'), ({'a': 0.9, 'b': 0.9}, 'refresh'), ({'a': 0.9, 'b': 0.3}, 'refresh'), ({'a': 0.78, 'b': 0.03}, 'refresh')]:
            self.assertEqual(routing.unique_relevant(probabilities, 0.8, 0.2), expected)
        for probabilities in [{}, {'a': float('nan')}, {'a': 1.1}]:
            with self.assertRaises(ValueError):
                routing.unique_relevant(probabilities, 0.8, 0.2)

    def test_exact_resolver_survives_240_order_permutations(self):
        rng = random.Random(20261001)
        for case in routing.browser.cases():
            for _ in range(10):
                rng.shuffle(case['observation'])
                descriptions, actions = routing.candidates(case)
                chosen = routing.exact_resolver(case['goal'], descriptions, actions)
                self.assertEqual(actions[chosen], case['expected'])
                for key, description in descriptions.items():
                    if key == "refresh":
                        continue
                    self.assertNotIn(' hidden', description)
                    self.assertNotIn(' disabled', description)
                    self.assertNotIn(' stale', description)

    def test_semantic_abstention_is_not_labelled_as_wrong_action(self):
        cases = routing.build_cases()
        semantic = cases[24:]
        self.assertTrue(all(case['baseline'] == 'refresh' for case in semantic))
        self.assertEqual(sum(case['baseline'] == case['expected'] for case in semantic), 5)
        self.assertEqual(next(case['expected'] for case in semantic if case['id'] == 'next-page'), 'refresh')


if __name__ == '__main__':
    unittest.main()
