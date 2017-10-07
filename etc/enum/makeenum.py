import yaml
import sys
import stringcase


def read_spec(filename):
    with open(filename) as f:
        return yaml.load(f)


def output_foreach_code(spec, tmpl):
    for value, label in spec['codes'].items():
        print tmpl.format(
            label=label,
            label_pascal=stringcase.pascalcase(label.lower()),
            value=value,
            **spec
        )


def output(spec):
    spec['string_map_var'] = 'strings' + spec['name']
    spec['receiver'] = spec['name'][0].lower()

    lines = [
        '// Code generated by etc/enum/makeenum.py',
        '',
        'package {pkg}',
        '',
        '// {doc}',
        'type {name} {type}',
        '',
        '// All possible {name} values.',
        '//',
        '// Insert: {ref}',
    ]
    for line in lines:
        print line.format(**spec)

    print 'const ('
    output_foreach_code(spec, '\t{label_pascal} {name} = {value}')
    print ')'
    print

    print 'var {string_map_var} = map[{name}]string{{'.format(**spec)
    output_foreach_code(spec, '\t{value}: "{label}",')
    print '}'


    print '''
    func ({receiver} {name}) String() string {{
        s, ok := {string_map_var}[{receiver}]
        if ok {{
            return s
        }}
        return fmt.Sprintf("{name}(%d)", {type}({receiver}))
    }}
    '''.format(**spec)

    print '''
    func Is{name}({receiver} {type}) bool {{
        _, ok := {string_map_var}[{name}({receiver})]
        return ok
    }}
    '''.format(**spec)



#var commandStrings = map[Command]string{
#	0:   "PADDING",
#	1:   "CREATE",
#	2:   "CREATED",
#	3:   "RELAY",
#	4:   "DESTROY",
#	5:   "CREATE_FAST",
#	6:   "CREATED_FAST",
#	8:   "NETINFO",
#	9:   "RELAY_EARLY",
#	10:  "CREATE2",
#	11:  "CREATED2",
#	7:   "VERSIONS",
#	128: "VPADDING",
#	129: "CERTS",
#	130: "AUTH_CHALLENGE",
#	131: "AUTHENTICATE",
#	132: "AUTHORIZE",
#}
#
#func (c Command) String() string {
#	s, ok := commandStrings[c]
#	if ok {
#		return s
#	}
#	return fmt.Sprintf("Command(%d)", byte(c))
#}
#
#// IsCommand determines whether the given byte is a recognized cell command.
#func IsCommand(c byte) bool {
#	_, ok := commandStrings[Command(c)]
#	return ok
#}


def main(args):
    spec = read_spec(args[0])
    output(spec)


if __name__ == '__main__':
    main(sys.argv[1:])
