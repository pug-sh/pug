import { useState } from 'react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Plus, Minus } from 'lucide-react';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
import type { FilterGroupUI, ConditionUI } from '@/pages/segments/segments';
import {
  addConditionToGroup,
  addSubGroupToGroup,
  removePartFromGroup,
  updateGroupOperator,
  generateId
} from '@/lib/segments';

interface GroupEditorProps {
  group: FilterGroupUI;
  setGroup: (group: FilterGroupUI) => void;
  level?: number;
}

export function GroupEditor({ group, setGroup, level = 0 }: GroupEditorProps) {
  const [activeGroup, setActiveGroup] = useState<FilterGroupUI>(group);

  const isRootGroup = level === 0;

  const addNewCondition = (groupId: string) => {
    const newCondition: ConditionUI = {
      id: generateId(),
      field: '',
      operator: 'EQUALS',
      value: ''
    };
    const updatedGroup = addConditionToGroup(groupId, newCondition, activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const addNewSubGroup = (groupId: string) => {
    const newSubGroup: FilterGroupUI = {
      id: generateId(),
      parts: [],
      logicalOperator: 'AND',
      isNested: true
    };
    const updatedGroup = addSubGroupToGroup(groupId, newSubGroup, activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const handleRemovePartFromGroup = (groupId: string, partId: string) => {
    const updatedGroup = removePartFromGroup(groupId, partId, activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const handleUpdateGroupOperator = (groupId: string, operator: 'AND' | 'OR') => {
    const updatedGroup = updateGroupOperator(groupId, operator, activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const handleChangeField = (conditionId: string, value: string) => {
    const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
      if (currentGroup.id === group.id) {
        const updatedParts = currentGroup.parts.map(part => {
          if ('id' in part && part.id === conditionId) {
            return { ...part, field: value };
          }
          if ('isNested' in part && part.isNested) {
            return updateGroup(part as FilterGroupUI);
          }
          return part;
        });
        return { ...currentGroup, parts: updatedParts };
      }

      return {
        ...currentGroup,
        parts: currentGroup.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      };
    };

    const updatedGroup = updateGroup(activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const handleChangeOperator = (conditionId: string, value: string) => {
    const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
      if (currentGroup.id === group.id) {
        const updatedParts = currentGroup.parts.map(part => {
          if ('id' in part && part.id === conditionId) {
            return { ...part, operator: value };
          }
          if ('isNested' in part && part.isNested) {
            return updateGroup(part as FilterGroupUI);
          }
          return part;
        });
        return { ...currentGroup, parts: updatedParts };
      }

      return {
        ...currentGroup,
        parts: currentGroup.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      };
    };

    const updatedGroup = updateGroup(activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  const handleChangeValue = (conditionId: string, value: string) => {
    const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
      if (currentGroup.id === group.id) {
        const updatedParts = currentGroup.parts.map(part => {
          if ('id' in part && part.id === conditionId) {
            return { ...part, value: value };
          }
          if ('isNested' in part && part.isNested) {
            return updateGroup(part as FilterGroupUI);
          }
          return part;
        });
        return { ...currentGroup, parts: updatedParts };
      }

      return {
        ...currentGroup,
        parts: currentGroup.parts.map(part =>
          'isNested' in part ? updateGroup(part as FilterGroupUI) : part
        )
      };
    };

    const updatedGroup = updateGroup(activeGroup);
    setActiveGroup(updatedGroup);
    setGroup(updatedGroup);
  };

  return (
    <div
      className={`p-4 rounded-lg ${isRootGroup ? 'bg-white' : 'bg-gray-50'} border`}
    >
      <div className="flex items-center justify-between mb-3">
        <h3 className="font-medium">
          {isRootGroup ? 'Main Group' : `Nested Group ${level}`}
        </h3>
        <div className="flex items-center space-x-2">
          <span className="text-sm text-muted-foreground">Operator:</span>
          <Select
            value={activeGroup.logicalOperator}
            onValueChange={(value) => handleUpdateGroupOperator(activeGroup.id, value as 'AND' | 'OR')}
          >
            <SelectTrigger className="w-[80px]">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="AND">AND</SelectItem>
              <SelectItem value="OR">OR</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      <div className="space-y-3">
        {activeGroup.parts.map((part, index) => {
          const partKey = part.id || `part-${index}`;

          if ('isNested' in part && part.isNested) {
            // It's a sub-group
            return (
              <div key={partKey} className="ml-4 pl-4 border-l-2 border-gray-300">
                <GroupEditor 
                  group={part as FilterGroupUI} 
                  setGroup={(updatedSubGroup) => {
                    const updateGroup = (currentGroup: FilterGroupUI): FilterGroupUI => {
                      if (currentGroup.id === activeGroup.id) {
                        const updatedParts = currentGroup.parts.map(p => 
                          ('id' in p && p.id === part.id) ? updatedSubGroup : p
                        );
                        return { ...currentGroup, parts: updatedParts };
                      }

                      return {
                        ...currentGroup,
                        parts: currentGroup.parts.map(p =>
                          'isNested' in p ? updateGroup(p as FilterGroupUI) : p
                        )
                      };
                    };

                    const updatedGroup = updateGroup(activeGroup);
                    setActiveGroup(updatedGroup);
                    setGroup(updatedGroup);
                  }}
                  level={level + 1}
                />
                <div className="mt-2 flex justify-end">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() => handleRemovePartFromGroup(activeGroup.id, part.id)}
                  >
                    <Minus className="h-4 w-4" />
                  </Button>
                </div>
              </div>
            );
          } else {
            // It's a condition
            const condition = part as ConditionUI;
            return (
              <div
                key={partKey}
                className="flex items-center space-x-2 p-2 bg-white rounded border"
              >
                <Input
                  placeholder="Field (e.g. gender)"
                  value={condition.field}
                  onChange={(e) => handleChangeField(condition.id, e.target.value)}
                />
                <Select
                  value={condition.operator}
                  onValueChange={(value) => handleChangeOperator(condition.id, value)}
                >
                  <SelectTrigger className="w-[180px]">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="EQUALS">equals</SelectItem>
                    <SelectItem value="NOT_EQUALS">not equals</SelectItem>
                    <SelectItem value="CONTAINS">contains</SelectItem>
                    <SelectItem value="NOT_CONTAINS">does not contain</SelectItem>
                    <SelectItem value="GREATER_THAN">greater than</SelectItem>
                    <SelectItem value="LESS_THAN">less than</SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  placeholder="Value"
                  value={condition.value}
                  onChange={(e) => handleChangeValue(condition.id, e.target.value)}
                />
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() => handleRemovePartFromGroup(activeGroup.id, condition.id)}
                >
                  <Minus className="h-4 w-4" />
                </Button>
              </div>
            );
          }
        })}

        <div className="flex space-x-2 mt-3">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => addNewCondition(activeGroup.id)}
          >
            <Plus className="h-4 w-4 mr-1" />
            Add Condition
          </Button>
          {level < 3 && ( // Limit nesting to 3 levels to prevent infinite nesting
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => addNewSubGroup(activeGroup.id)}
            >
              <Plus className="h-4 w-4 mr-1" />
              Add Group
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}