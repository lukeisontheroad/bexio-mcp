export default {
  extends: ["@commitlint/config-conventional"],
  // Dependabot's generated messages trip subject-case; exempt them rather
  // than relax the rule for authored commits.
  ignores: [(message) => /^Signed-off-by: dependabot\[bot\]/m.test(message)],
};
