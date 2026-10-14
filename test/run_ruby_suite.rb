# frozen_string_literal: true

require 'digest'
require 'fileutils'
require 'json'
require 'optparse'

options = { shard: '0/1' }
OptionParser.new do |parser|
  parser.on('--shard INDEX/COUNT') { |value| options[:shard] = value }
  parser.on('--timings FILE') { |value| options[:timings] = value }
  parser.on('--list') { options[:list] = true }
end.order!
suite = ARGV.shift
unless suite && suite.match?(/\A[a-z][a-z0-9_]*_test\.rb\z/) && File.file?(File.join(__dir__, suite))
  abort 'use run_ruby_suite.rb [--shard INDEX/COUNT] [--timings FILE] SUITE_test.rb'
end
match = /\A(\d+)\/(\d+)\z/.match(options[:shard])
abort 'invalid shard' unless match && match[2].to_i.between?(1, 32) && match[1].to_i < match[2].to_i
index, count = match.captures.map(&:to_i)
require_relative suite
Minitest.seed = 1
identities = Minitest::Runnable.runnables.flat_map { |klass| klass.runnable_methods.map { |name| "#{klass.name}##{name}" } }.sort
selected = identities.select { |identity| Digest::SHA256.hexdigest(identity).to_i(16) % count == index }

if options[:list]
  puts JSON.generate('schema' => 1, 'suite' => suite, 'all' => identities, 'selected' => selected)
  # Listing must not trigger minitest/autorun's at_exit execution.
  STDOUT.flush
  exit! 0
end

class WorkspaceTimingReporter < Minitest::AbstractReporter
  def initialize(path, suite, all, selected, shard)
    @path, @suite, @all, @selected, @shard = path, suite, all, selected, shard
    @results = []
  end

  def record(result)
    @results << {'identity' => "#{result.klass}##{result.name}", 'seconds' => result.time,
      'assertions' => result.assertions, 'skipped' => result.skipped?, 'passed' => result.passed?}
  end

  def passed?
    @results.map { |result| result['identity'] }.sort == @selected
  end

  def report
    warn 'Ruby shard did not execute its complete test manifest' unless passed?
    return unless @path
    FileUtils.mkdir_p(File.dirname(@path))
    File.write(@path, JSON.pretty_generate('schema' => 1, 'suite' => @suite, 'shard' => @shard,
      'discovered' => @all, 'selected' => @selected, 'coverageComplete' => passed?, 'tests' => @results) + "\n")
  end
end

Minitest.extensions << 'workspace_timing'
Minitest.define_singleton_method(:plugin_workspace_timing_init) do |_settings|
  reporter << WorkspaceTimingReporter.new(options[:timings], suite, identities, selected, options[:shard])
end
ARGV.concat(['--name', "/\\A(?:#{selected.map { |identity| Regexp.escape(identity) }.join('|')})\\z/"])
