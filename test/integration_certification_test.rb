# frozen_string_literal: true

require 'minitest/autorun'
require 'tmpdir'
require_relative '../libexec/workspace-coordination-execution'
require_relative 'support/coordination_fixture'

class IntegrationCertificationTest < Minitest::Test
  include CoordinationFixture

  def test_rebase_over_tracking_changes_reconstructs_the_exact_feature
    first = owner('certify')
    checkout = feature(first)
    original = git('-C', checkout, 'rev-parse', 'HEAD').strip
    git('-C', checkout, 'checkout', '-b', 'baseline', @base)
    FileUtils.mkdir_p(File.join(checkout, 'work/example'))
    File.write(File.join(checkout, 'work/example/state.md'), 'tracking')
    git('-C', checkout, 'add', '.')
    git('-C', checkout, 'commit', '-m', 'tracking fixture')
    baseline = git('-C', checkout, 'rev-parse', 'HEAD').strip
    git('-C', checkout, 'checkout', first['slug'])
    git('-C', checkout, 'rebase', baseline)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    proof = WorkspaceCoordination::Certification.new(@engine).prove(@bare,
      from_base: @base, from: original, to_base: baseline, to: head)
    assert_equal 'reconstructed_patch', proof['relation']
    assert_equal proof['tree'], proof['reconstructedTree']
  end

  def test_absorbed_patch_certifies_identical_tree_but_removed_effect_does_not
    first = owner('absorbed')
    checkout = feature(first)
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    certification = WorkspaceCoordination::Certification.new(@engine)
    proof = certification.prove(@bare, from_base: @base, from: head, to_base: head, to: head)
    assert_equal 'identical_tree', proof['relation']
    File.delete(File.join(checkout, first['slug']))
    git('-C', checkout, 'add', '.')
    git('-C', checkout, 'commit', '-m', 'revert absorbed effect')
    reverted = git('-C', checkout, 'rev-parse', 'HEAD').strip
    assert_raises(WorkspaceCoordination::Error) do
      certification.prove(@bare, from_base: @base, from: head, to_base: reverted, to: reverted)
    end
  end

  def test_matching_tree_does_not_exempt_migration_changes
    first = owner('migration')
    checkout = feature(first)
    FileUtils.mkdir_p(File.join(checkout, 'db/migrate'))
    File.write(File.join(checkout, 'db/migrate/001.rb'), 'schema change')
    git('-C', checkout, 'add', '.')
    git('-C', checkout, 'commit', '-m', 'migration fixture')
    head = git('-C', checkout, 'rev-parse', 'HEAD').strip
    assert_raises(WorkspaceCoordination::Error) do
      WorkspaceCoordination::Certification.new(@engine).prove(@bare,
        from_base: @base, from: head, to_base: head, to: head)
    end
  end

  def test_check_reuse_requires_the_derivation_and_every_input
    receipt = {'schema' => 1, 'exitStatus' => 0, 'derivation' => '/nix/store/example.drv', 'inputs' => {'source' => 'abc', 'tool' => 'ruby'}}
    assert WorkspaceCoordination::Certification.reusable_check?(receipt, derivation: receipt['derivation'], inputs: receipt['inputs'])
    refute WorkspaceCoordination::Certification.reusable_check?(receipt, derivation: '/nix/store/other.drv', inputs: receipt['inputs'])
    refute WorkspaceCoordination::Certification.reusable_check?(receipt, derivation: receipt['derivation'], inputs: {'source' => 'abc', 'tool' => 'new-ruby'})
  end
end
