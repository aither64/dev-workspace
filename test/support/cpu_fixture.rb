# frozen_string_literal: true

# Only the opt-in Go fixture invokes this adapter, with its own temporary root.
require 'json'
require 'fileutils'

mode, root = ARGV.shift(2)
raise 'CPU fixture requires its private marker' unless root && File.file?(File.join(root, 'cpu-fixture'))

if mode == 'notify'
  request = JSON.parse(File.binread(ARGV.last))
  File.open(File.join(root, 'helpers.log'), 'a', 0o600) { |f| f.puts("notify-#{request.fetch('action')}") }
  puts JSON.generate('options' => {}, 'receipt' => {'threadId' => request.fetch('thread')})
  exit
end

require_relative '../../libexec/workspace-coordination-execution'
require_relative '../../libexec/workspace-integration'
load File.expand_path('../../libexec/workspace-host', __dir__)

workspace = File.join(root, 'workspace')
package = File.join(root, 'package')
state = File.join(root, 'state')
profile = File.join(root, 'profile')
entry = {'name' => 'cpu-fixture', 'root' => workspace, 'socket' => File.join(root, 'native.sock'),
         'authority' => File.join(root, 'authority'), 'portalUrl' => 'https://fixture.example.test'}
engine = WorkspaceCoordination::Engine.new(state:, package:, profile:, workspace:, entry:, fixtures: true)

if mode == 'setup'
  data = engine.store.empty
  Dir[File.join(workspace, 'work', '*')].sort.each do |directory|
    owner = engine.verified_owner_tracking(File.basename(directory))
    data['owners'][owner['id']] = owner
  end
  target = Integer(ARGV.fetch(0))
  while JSON.generate(data).bytesize < target
    data['receipts'][SecureRandom.uuid] = {'digest' => 'sha256:' + '0' * 64,
      'result' => {'synthetic' => 'x' * [12_000, target - JSON.generate(data).bytesize].min}}
  end
  engine.store.validate!(data)
  WorkspaceCoordination.atomic(File.join(engine.store.root, 'ledger.json'), data)
  puts JSON.generate('owners' => data['owners'].length,
    'ledgerBytes' => File.size(File.join(engine.store.root, 'ledger.json')))
  exit
end

if mode == 'status'
  File.open(File.join(root, 'helpers.log'), 'a', 0o600) { |f| f.puts('status') }
  puts JSON.generate(engine.status(nil, portal: true))
  exit
end
raise 'unsupported CPU fixture mode' unless mode == 'run'

counts = Hash.new(0)
%i[read claims integration_journals].each do |name|
  original = engine.store.method(name)
  engine.store.define_singleton_method(name) { counts[name] += 1; original.call }
end
native = engine.method(:notification_call)
engine.define_singleton_method(:notification_call) do |owner, event, action|
  counts["native_#{action}"] += 1
  native.call(owner, event, action)
end
# Keep the real native verification subprocess path, but never launch systemd,
# nix-store or a real recipient. The selected fixture helper returns fixed JSON.
engine.instance_variable_set(:@fixtures, false)
engine.define_singleton_method(:retain_root) { |*_args| nil }
host = Object.new.extend(DevWorkspaceHost::WorkspaceCoordinationHost)
host.instance_variable_set(:@profile, profile)
host.instance_variable_set(:@err, $stderr)
host.define_singleton_method(:package_root) { package }
host.define_singleton_method(:registry) { Struct.new(:entries).new([entry]) }
host.define_singleton_method(:coordination_engine) { |_entry| engine }
host.define_singleton_method(:with_coordination_session_admission) { |*_args, **_options, &block| block.call }
capture = engine.method(:capture)
engine.define_singleton_method(:capture) do |*argv, **options|
  if argv.first == 'systemctl'
    "inactive\n"
  elsif argv.first == 'systemd-run'
    host.coordination_notifications_command(['--request', argv.last])
    ''
  else
    capture.call(*argv, **options)
  end
end

clock = -> { Process.clock_gettime(Process::CLOCK_MONOTONIC) }
cpu = -> { Process.times.to_a.sum }
self_cpu = -> { value = Process.times; value.utime + value.stime }
started, baseline_cpu, baseline_self, baseline_alloc = clock.call, cpu.call, self_cpu.call, GC.stat(:total_allocated_objects)
deliveries = []
pending = {}
wake_path = File.join(engine.store.root, 'wake.sock')
wake = Socket.new(Socket::AF_UNIX, Socket::SOCK_DGRAM, 0)
wake.bind(Socket.sockaddr_un(wake_path))
$stdout.sync = true
read_command = lambda do
  value = +''
  value << $stdin.sysread(1) until value.end_with?("\n")
  value.strip
rescue EOFError
  nil
end
puts JSON.generate('ready' => true)
loop do
  # The baseline adapter is the old run_coordinator per-workspace traversal.
  # Candidate calls the actual new per-workspace entry point, without collection.
  if host.respond_to?(:coordinate_workspace)
    host.coordinate_workspace(entry, engine)
  else
    engine.reconcile_attempts
    engine.store.read['owners'].values.each do |owner|
      host.coordinate_owner(entry, engine, owner) unless owner['fence']
    end
  end
  unless pending.empty?
    delivered = engine.store.read['outbox']
    pending.delete_if do |id, time|
      next false unless delivered.fetch(id)['state'] == 'delivered'
      deliveries << (clock.call - time) * 1000
      true
    end
  end
  readable = IO.select([$stdin, wake], nil, nil, 5)&.first || []
  wake.recv_nonblock(64) while IO.select([wake], nil, nil, 0)
  next unless readable.include?($stdin)
  command = read_command.call
  break unless command
  case command
  when 'start'
    counts.clear
    deliveries.clear
    started, baseline_cpu, baseline_self, baseline_alloc = clock.call, cpu.call, self_cpu.call, GC.stat(:total_allocated_objects)
    puts JSON.generate('started' => true)
  when 'event'
    owner = engine.store.read['owners'].values.first
    id = SecureRandom.uuid
    pending[id] = clock.call
    engine.store.transaction do |data|
      data['outbox'][id] = {'id' => id, 'ownerId' => owner['id'], 'thread' => owner['thread'],
        'text' => 'Synthetic benchmark notification.', 'messageId' => SecureRandom.uuid,
        'actionContext' => "coordination:#{owner['id']}:#{id}", 'state' => 'pending',
        'options' => nil, 'receipt' => nil, 'attempts' => 0, 'retryAt' => 0}
    end
    puts JSON.generate('queued' => id)
  when 'stop'
    puts JSON.generate('seconds' => clock.call - started, 'cpuSecondsIncludingChildren' => cpu.call - baseline_cpu,
      'selfCPUSeconds' => self_cpu.call - baseline_self,
      'allocations' => GC.stat(:total_allocated_objects) - baseline_alloc,
      'calls' => counts, 'deliveryMS' => deliveries, 'pendingDeliveries' => pending.length)
    break
  else
    raise "unknown fixture command #{command}"
  end
end
wake.close
File.unlink(wake_path)
