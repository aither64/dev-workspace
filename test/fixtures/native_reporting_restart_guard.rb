# frozen_string_literal: true

# Disposable native integration only: systemd/cgroup membership is supplied by
# the fixture. Executable, argv and parent identities come from real /proc data.
load ARGV.shift
workspace, state, profile, package, native, parent, argv = ARGV
parent = Integer(parent, 10)
host = DevWorkspaceHost::Host.allocate
host.instance_variable_set(:@state, state)
host.instance_variable_set(:@profile, profile)
host.instance_variable_set(:@runtime, File.join(workspace, 'runtime'))
host.define_singleton_method(:package_root) { package }
host.define_singleton_method(:provider_native_binary) { File.realpath(native) }
host.define_singleton_method(:capture!) do |*arguments|
  expected = ['systemctl', '--user', 'show', 'workspace-codex@fixture.service', '--property=MainPID,ControlGroup']
  raise 'unexpected system observation' unless arguments == expected

  "MainPID=#{parent}\nControlGroup=/fixture.service\n"
end
entry = {'name' => 'fixture', 'root' => workspace}
plan = DevWorkspaceHost::RegistrationPlan.new(package_root: package, argv: JSON.parse(argv),
  digest: 'a' * 64, policy: 1, required_native_child_threads: 0, states: [])
host.send(:write_registration_marker, entry, native, plan)
children = Dir.glob('/proc/[0-9]*/status').filter_map do |path|
  pid = Integer(path.split('/')[2], 10)
  pid if host.send(:provider_process_parent, pid) == parent
rescue Errno::ENOENT, Errno::ESRCH
  nil
end
owned = children.select { |pid| host.send(:provider_process_binary, pid) == File.join(package, 'bin/workspace-portal') }
raise 'native did not launch actual reporting helpers' if owned.empty?
raise 'native did not preserve unrelated MCP helpers' if (children - owned).empty?

processes = [parent, *owned]
host.define_singleton_method(:provider_cgroup_processes) { |_| processes }
inventory = {'blockers' => [], 'threads' => []}
host.send(:require_provider_restart_safe!, entry, inventory)
processes = [parent, *children]
begin
  host.send(:require_provider_restart_safe!, entry, inventory)
  raise 'unrelated MCP helper incorrectly accepted'
rescue DevWorkspaceHost::Error => e
  raise unless e.message.include?('additional or unproved processes')
end
puts "proved #{owned.length} actual daemon reporting helper(s); unrelated MCP blocks restart"
