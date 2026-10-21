# frozen_string_literal: true

# Explicit post-review check. Install Overcommit 0.73.0 in a disposable GEM_HOME
# and set COORDINATION_OVERCOMMIT to its executable. Never uses existing trust.
require_relative 'coordination_claims_test'

class CoordinationClaimsTest
  def test_overcommit_preparation_and_repairs_stay_outside_merge_claims
    overcommit = ENV['COORDINATION_OVERCOMMIT']
    skip 'explicit disposable Overcommit installation required' unless overcommit
    version, error, status = Open3.capture3(overcommit, '--version')
    assert status.success?, error
    assert_includes version, '0.73.0'
    person = owner('signed-hooks')
    feature = feature(person)
    hook_types = %w[post_checkout post_merge pre_push]
    hook_types.each do |type|
      name = type.split('_').map(&:capitalize).join
      directory = File.join(feature, '.git-hooks', type)
      FileUtils.mkdir_p(directory)
      File.write(File.join(directory, 'probe.rb'), <<~RUBY)
        module Overcommit::Hook::#{name}
          class Probe < Base
            def run
              common = IO.popen(['git', 'rev-parse', '--path-format=absolute', '--git-common-dir'], &:read).strip
              File.open(File.join(common, 'probe.log'), 'a') { |file| file.puts('#{type}') }
              return [:fail, 'fixture deliberate failure'] if File.exist?(File.join(common, 'fail_#{type}'))
              :pass
            end
          end
        end
      RUBY
    end
    config = <<~YAML
      verify_signatures: true
      PreCommit:
        ALL:
          enabled: false
      CommitMsg:
        ALL:
          enabled: false
      PostCheckout:
        ALL:
          enabled: false
        Probe:
          enabled: true
          description: baseline configuration
      PostMerge:
        ALL:
          enabled: false
        Probe:
          enabled: true
      PrePush:
        ALL:
          enabled: false
        Probe:
          enabled: true
    YAML
    File.write(File.join(feature, '.overcommit.yml'), config)
    git('-C', feature, 'add', '.git-hooks', '.overcommit.yml')
    git('-C', feature, 'commit', '-m', 'fixture baseline hooks')
    baseline = git('-C', feature, 'rev-parse', 'HEAD').strip
    File.write(File.join(feature, '.overcommit.yml'), config.sub('baseline configuration', 'feature configuration'))
    git('-C', feature, 'commit', '-am', 'fixture feature hooks')
    head = git('-C', feature, 'rev-parse', 'HEAD').strip
    original_base = git('-C', @bare, 'rev-parse', 'master').strip
    assert_empty @engine.store.held_claim_resources
    outputs = {}
    # These are ordinary session tools, never coordinator-executed commands.
    run = lambda do |cwd, argv|
      out, err, status = Open3.capture3(*argv, chdir: cwd)
      outputs['last'] = out + err
      {'state' => status.success? ? 'succeeded' : 'failed'}
    end
    assert_equal 'succeeded', run.call(feature, [overcommit, '--install'])['state']
    hook_types.each { |type| assert_equal 'succeeded', run.call(feature, [overcommit, '--sign', type.tr('_', '-')])['state'] }
    assert_equal 'succeeded', run.call(feature, ['git', 'checkout', person['slug']])['state']
    target = File.join(@tmp, 'signed-target')
    failed = run.call(feature, ['git', 'worktree', 'add', '--detach', target, baseline])
    assert_equal 'failed', failed['state']
    assert_includes outputs['last'], 'Signature of configuration file has changed!'
    assert File.directory?(target)
    assert_equal 'succeeded', run.call(target, [overcommit, '--sign'])['state']
    hook_types.each { |type| assert_equal 'succeeded', run.call(target, [overcommit, '--sign', type.tr('_', '-')])['state'] }
    assert_equal 'succeeded', run.call(target, ['git', 'checkout', '--detach', baseline])['state']
    assert_equal 'failed', run.call(feature, ['git', 'checkout', person['slug']])['state']
    assert_equal 'succeeded', run.call(feature, [overcommit, '--sign'])['state']
    hook_types.each { |type| assert_equal 'succeeded', run.call(feature, [overcommit, '--sign', type.tr('_', '-')])['state'] }
    assert_equal 'succeeded', run.call(feature, ['git', 'checkout', person['slug']])['state']
    assert_equal original_base, git('-C', @bare, 'rev-parse', 'master').strip
    assert_empty @engine.store.held_claim_resources
    merge_blocker = File.join(@bare, 'fail_post_merge')
    File.write(merge_blocker, 'deliberate fixture hook failure')
    claim = acquire(person)
    assert_equal 'held', claim['state']
    assert_equal original_base, git('--git-dir', @remote, 'rev-parse', 'master').strip
    # Git returns zero even when its post-merge hook fails. The session must
    # inspect hook diagnostics before deciding whether publication is safe.
    assert_equal 'succeeded', run.call(target, ['git', 'merge', '--ff-only', head])['state']
    assert_includes outputs['last'], 'fixture deliberate failure'
    assert_equal head, git('-C', target, 'rev-parse', 'HEAD').strip
    assert_equal original_base, git('--git-dir', @remote, 'rev-parse', 'master').strip
    @engine.change_claim(person, claim['id'], 'release')
    File.unlink(merge_blocker)
    assert_equal 'succeeded', run.call(target, ['git', 'hook', 'run', 'post-merge', '--', '0'])['state']
    assert_empty @engine.store.held_claim_resources
    blocker = File.join(@bare, 'fail_pre_push')
    File.write(blocker, 'deliberate fixture hook failure')
    claim = acquire(person)
    result = run.call(target, ['git', 'push', 'origin', "#{head}:refs/heads/master"])
    assert_equal 'failed', result['state']
    assert_includes outputs['last'], 'fixture deliberate failure'
    assert_equal original_base, git('--git-dir', @remote, 'rev-parse', 'master').strip
    assert_equal head, git('-C', target, 'rev-parse', 'HEAD').strip
    @engine.change_claim(person, claim['id'], 'release')
    File.unlink(blocker)
    assert_empty @engine.store.held_claim_resources
    claim = acquire(person)
    assert_equal original_base, git('--git-dir', @remote, 'rev-parse', 'master').strip
    assert_equal 'succeeded', run.call(target, ['git', 'push', 'origin', "#{head}:refs/heads/master"])['state']
    assert_equal head, git('--git-dir', @remote, 'rev-parse', 'master').strip
    @engine.change_claim(person, claim['id'], 'release')
    calls = File.readlines(File.join(@bare, 'probe.log'), chomp: true)
    assert_equal %w[post_checkout post_checkout post_checkout post_merge post_merge pre_push pre_push], calls
    assert_empty @engine.store.held_claim_resources
    refute File.exist?(File.join(@engine.store.root, 'sources'))
  end
end
